package ipfscluster

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	rpc "github.com/hsanjuan/go-libp2p-gorpc"
	cid "github.com/ipfs/go-cid"
	peer "github.com/libp2p/go-libp2p-peer"
	ma "github.com/multiformats/go-multiaddr"

	mux "github.com/gorilla/mux"
)

// Server settings
var (
	// maximum duration before timing out read of the request
	RESTAPIServerReadTimeout = 5 * time.Second
	// maximum duration before timing out write of the response
	RESTAPIServerWriteTimeout = 10 * time.Second
	// server-side the amount of time a Keep-Alive connection will be
	// kept idle before being reused
	RESTAPIServerIdleTimeout = 60 * time.Second
)

// RESTAPI implements an API and aims to provides
// a RESTful HTTP API for Cluster.
type RESTAPI struct {
	ctx        context.Context
	apiAddr    ma.Multiaddr
	listenAddr string
	listenPort int
	rpcClient  *rpc.Client
	rpcReady   chan struct{}
	router     *mux.Router

	listener net.Listener
	server   *http.Server

	shutdownLock sync.Mutex
	shutdown     bool
	wg           sync.WaitGroup
}

type route struct {
	Name        string
	Method      string
	Pattern     string
	HandlerFunc http.HandlerFunc
}

type errorResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e errorResp) Error() string {
	return e.Message
}

type versionResp struct {
	Version string `json:"version"`
}

type pinResp struct {
	Pinned string `json:"pinned"`
}

type unpinResp struct {
	Unpinned string `json:"unpinned"`
}

type statusInfo struct {
	IPFS  string `json:"ipfs"`
	Error string `json:"error,omitempty"`
}

type statusCidResp struct {
	Cid    string                `json:"cid"`
	Status map[string]statusInfo `json:"status"`
}

type idResp struct {
	ID                 string   `json:"id"`
	PublicKey          string   `json:"public_key"`
	Addresses          []string `json:"addresses"`
	Version            string   `json:"version"`
	Commit             string   `json:"commit"`
	RPCProtocolVersion string   `json:"rpc_protocol_version"`
}

type statusResp []statusCidResp

// NewRESTAPI creates a new object which is ready to be
// started.
func NewRESTAPI(cfg *Config) (*RESTAPI, error) {
	ctx := context.Background()

	listenAddr, err := cfg.APIAddr.ValueForProtocol(ma.P_IP4)
	if err != nil {
		return nil, err
	}
	listenPortStr, err := cfg.APIAddr.ValueForProtocol(ma.P_TCP)
	if err != nil {
		return nil, err
	}
	listenPort, err := strconv.Atoi(listenPortStr)
	if err != nil {
		return nil, err
	}

	l, err := net.Listen("tcp", fmt.Sprintf("%s:%d",
		listenAddr, listenPort))
	if err != nil {
		return nil, err
	}

	router := mux.NewRouter().StrictSlash(true)
	s := &http.Server{
		ReadTimeout:  RESTAPIServerReadTimeout,
		WriteTimeout: RESTAPIServerWriteTimeout,
		//IdleTimeout:  RESTAPIServerIdleTimeout, // TODO: Go 1.8
		Handler: router,
	}
	s.SetKeepAlivesEnabled(true) // A reminder that this can be changed

	api := &RESTAPI{
		ctx:        ctx,
		apiAddr:    cfg.APIAddr,
		listenAddr: listenAddr,
		listenPort: listenPort,
		listener:   l,
		server:     s,
		rpcReady:   make(chan struct{}, 1),
	}

	for _, route := range api.routes() {
		router.
			Methods(route.Method).
			Path(route.Pattern).
			Name(route.Name).
			Handler(route.HandlerFunc)
	}

	api.router = router
	api.run()
	return api, nil
}

func (api *RESTAPI) routes() []route {
	return []route{
		{
			"ID",
			"GET",
			"/id",
			api.idHandler,
		},
		{
			"Members",
			"GET",
			"/members",
			api.memberListHandler,
		},
		{
			"Pins",
			"GET",
			"/pins",
			api.pinListHandler,
		},
		{
			"Version",
			"GET",
			"/version",
			api.versionHandler,
		},
		{
			"Pin",
			"POST",
			"/pins/{hash}",
			api.pinHandler,
		},
		{
			"Unpin",
			"DELETE",
			"/pins/{hash}",
			api.unpinHandler,
		},
		{
			"Status",
			"GET",
			"/status",
			api.statusHandler,
		},
		{
			"StatusCid",
			"GET",
			"/status/{hash}",
			api.statusCidHandler,
		},
		{
			"Sync",
			"POST",
			"/status",
			api.syncHandler,
		},
		{
			"SyncCid",
			"POST",
			"/status/{hash}",
			api.syncCidHandler,
		},
	}
}

func (api *RESTAPI) run() {
	api.wg.Add(1)
	go func() {
		defer api.wg.Done()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		api.ctx = ctx

		<-api.rpcReady

		logger.Infof("REST API: %s", api.apiAddr)
		err := api.server.Serve(api.listener)
		if err != nil && !strings.Contains(err.Error(), "closed network connection") {
			logger.Error(err)
		}
	}()
}

// Shutdown stops any API listeners.
func (api *RESTAPI) Shutdown() error {
	api.shutdownLock.Lock()
	defer api.shutdownLock.Unlock()

	if api.shutdown {
		logger.Debug("already shutdown")
		return nil
	}

	logger.Info("stopping Cluster API")

	close(api.rpcReady)
	// Cancel any outstanding ops
	api.server.SetKeepAlivesEnabled(false)
	api.listener.Close()

	api.wg.Wait()
	api.shutdown = true
	return nil
}

// SetClient makes the component ready to perform RPC
// requests.
func (api *RESTAPI) SetClient(c *rpc.Client) {
	api.rpcClient = c
	api.rpcReady <- struct{}{}
}

func (api *RESTAPI) idHandler(w http.ResponseWriter, r *http.Request) {
	idObj := ID{}
	err := api.rpcClient.Call("",
		"Cluster",
		"ID",
		struct{}{},
		&idObj)
	if checkRPCErr(w, err) {
		pubKey := ""
		if idObj.PublicKey != nil {
			keyBytes, err := idObj.PublicKey.Bytes()
			if err == nil {
				pubKey = base64.StdEncoding.EncodeToString(keyBytes)
			}
		}
		addrs := make([]string, len(idObj.Addresses), len(idObj.Addresses))
		for i, a := range idObj.Addresses {
			addrs[i] = a.String()
		}
		idResponse := idResp{
			ID:                 idObj.ID.Pretty(),
			PublicKey:          pubKey,
			Addresses:          addrs,
			Version:            idObj.Version,
			Commit:             idObj.Commit,
			RPCProtocolVersion: string(idObj.RPCProtocolVersion),
		}
		sendJSONResponse(w, 200, idResponse)
	}
}

func (api *RESTAPI) versionHandler(w http.ResponseWriter, r *http.Request) {
	var v string
	err := api.rpcClient.Call("",
		"Cluster",
		"Version",
		struct{}{},
		&v)

	if checkRPCErr(w, err) {
		sendJSONResponse(w, 200, versionResp{v})
	}
}

func (api *RESTAPI) memberListHandler(w http.ResponseWriter, r *http.Request) {
	var peers []peer.ID
	err := api.rpcClient.Call("",
		"Cluster",
		"MemberList",
		struct{}{},
		&peers)

	if checkRPCErr(w, err) {
		var strPeers []string
		for _, p := range peers {
			strPeers = append(strPeers, p.Pretty())
		}
		sendJSONResponse(w, 200, strPeers)
	}
}

func (api *RESTAPI) pinHandler(w http.ResponseWriter, r *http.Request) {
	if c := parseCidOrError(w, r); c != nil {
		err := api.rpcClient.Call("",
			"Cluster",
			"Pin",
			c,
			&struct{}{})
		if checkRPCErr(w, err) {
			sendAcceptedResponse(w)
		}
	}
}

func (api *RESTAPI) unpinHandler(w http.ResponseWriter, r *http.Request) {
	if c := parseCidOrError(w, r); c != nil {
		err := api.rpcClient.Call("",
			"Cluster",
			"Unpin",
			c,
			&struct{}{})
		if checkRPCErr(w, err) {
			sendAcceptedResponse(w)
		}
	}
}

func (api *RESTAPI) pinListHandler(w http.ResponseWriter, r *http.Request) {
	var pins []string
	err := api.rpcClient.Call("",
		"Cluster",
		"PinList",
		struct{}{},
		&pins)
	if checkRPCErr(w, err) {
		sendJSONResponse(w, 200, pins)
	}

}

func (api *RESTAPI) statusHandler(w http.ResponseWriter, r *http.Request) {
	var pinInfos []GlobalPinInfo
	err := api.rpcClient.Call("",
		"Cluster",
		"Status",
		struct{}{},
		&pinInfos)
	if checkRPCErr(w, err) {
		sendStatusResponse(w, http.StatusOK, pinInfos)
	}
}

func (api *RESTAPI) statusCidHandler(w http.ResponseWriter, r *http.Request) {
	if c := parseCidOrError(w, r); c != nil {
		var pinInfo GlobalPinInfo
		err := api.rpcClient.Call("",
			"Cluster",
			"StatusCid",
			c,
			&pinInfo)
		if checkRPCErr(w, err) {
			sendStatusCidResponse(w, http.StatusOK, pinInfo)
		}
	}
}

func (api *RESTAPI) syncHandler(w http.ResponseWriter, r *http.Request) {
	var pinInfos []GlobalPinInfo
	err := api.rpcClient.Call("",
		"Cluster",
		"GlobalSync",
		struct{}{},
		&pinInfos)
	if checkRPCErr(w, err) {
		sendStatusResponse(w, http.StatusAccepted, pinInfos)
	}
}

func (api *RESTAPI) syncCidHandler(w http.ResponseWriter, r *http.Request) {
	if c := parseCidOrError(w, r); c != nil {
		var pinInfo GlobalPinInfo
		err := api.rpcClient.Call("",
			"Cluster",
			"GlobalSyncCid",
			c,
			&pinInfo)
		if checkRPCErr(w, err) {
			sendStatusCidResponse(w, http.StatusOK, pinInfo)
		}
	}
}

func parseCidOrError(w http.ResponseWriter, r *http.Request) *CidArg {
	vars := mux.Vars(r)
	hash := vars["hash"]
	_, err := cid.Decode(hash)
	if err != nil {
		sendErrorResponse(w, 400, "error decoding Cid: "+err.Error())
		return nil
	}
	return &CidArg{hash}
}

// checkRPCErr takes care of returning standard error responses if we
// pass an error to it. It returns true when everythings OK (no error
// was handled), or false otherwise.
func checkRPCErr(w http.ResponseWriter, err error) bool {
	if err != nil {
		sendErrorResponse(w, 500, err.Error())
		return false
	}
	return true
}

func sendEmptyResponse(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

func sendAcceptedResponse(w http.ResponseWriter) {
	w.WriteHeader(http.StatusAccepted)
}

func sendJSONResponse(w http.ResponseWriter, code int, resp interface{}) {
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		panic(err)
	}
}

func sendErrorResponse(w http.ResponseWriter, code int, msg string) {
	errorResp := errorResp{code, msg}
	logger.Errorf("sending error response: %d: %s", code, msg)
	sendJSONResponse(w, code, errorResp)
}

func transformPinToStatusCid(p GlobalPinInfo) statusCidResp {
	s := statusCidResp{}
	s.Cid = p.Cid.String()
	s.Status = make(map[string]statusInfo)
	for k, v := range p.Status {
		s.Status[k.Pretty()] = statusInfo{
			IPFS:  v.IPFS.String(),
			Error: v.Error,
		}
	}
	return s
}

func sendStatusResponse(w http.ResponseWriter, code int, data []GlobalPinInfo) {
	pins := make(statusResp, 0, len(data))

	for _, d := range data {
		pins = append(pins, transformPinToStatusCid(d))
	}
	sendJSONResponse(w, code, pins)
}

func sendStatusCidResponse(w http.ResponseWriter, code int, data GlobalPinInfo) {
	st := transformPinToStatusCid(data)
	sendJSONResponse(w, code, st)
}