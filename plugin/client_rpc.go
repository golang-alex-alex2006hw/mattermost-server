// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

//go:generate go run interface_generator/main.go

package plugin

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/rpc"
	"os"
	"reflect"

	"github.com/hashicorp/go-plugin"
	"github.com/mattermost/mattermost-server/mlog"
	"github.com/mattermost/mattermost-server/model"
)

var hookNameToId map[string]int = make(map[string]int)

type hooksRPCClient struct {
	client      *rpc.Client
	log         *mlog.Logger
	muxBroker   *plugin.MuxBroker
	apiImpl     API
	implemented [TotalHooksId]bool
}

type hooksRPCServer struct {
	impl         interface{}
	muxBroker    *plugin.MuxBroker
	apiRPCClient *apiRPCClient
}

// Implements hashicorp/go-plugin/plugin.Plugin interface to connect the hooks of a plugin
type hooksPlugin struct {
	hooks   interface{}
	apiImpl API
	log     *mlog.Logger
}

func (p *hooksPlugin) Server(b *plugin.MuxBroker) (interface{}, error) {
	return &hooksRPCServer{impl: p.hooks, muxBroker: b}, nil
}

func (p *hooksPlugin) Client(b *plugin.MuxBroker, client *rpc.Client) (interface{}, error) {
	return &hooksRPCClient{client: client, log: p.log, muxBroker: b, apiImpl: p.apiImpl}, nil
}

type apiRPCClient struct {
	client *rpc.Client
	log    *mlog.Logger
}

type apiRPCServer struct {
	impl API
}

// Registering some types used by MM for encoding/gob used by rpc
func init() {
	gob.Register([]*model.SlackAttachment{})
	gob.Register([]interface{}{})
	gob.Register(map[string]interface{}{})
}

// These enforce compile time checks to make sure types implement the interface
// If you are getting an error here, you probably need to run `make pluginapi` to
// autogenerate RPC glue code
var _ plugin.Plugin = &hooksPlugin{}
var _ Hooks = &hooksRPCClient{}

//
// Below are specal cases for hooks or APIs that can not be auto generated
//

func (g *hooksRPCClient) Implemented() (impl []string, err error) {
	err = g.client.Call("Plugin.Implemented", struct{}{}, &impl)
	for _, hookName := range impl {
		if hookId, ok := hookNameToId[hookName]; ok {
			g.implemented[hookId] = true
		}
	}
	return
}

// Implemented replies with the names of the hooks that are implemented.
func (s *hooksRPCServer) Implemented(args struct{}, reply *[]string) error {
	ifaceType := reflect.TypeOf((*Hooks)(nil)).Elem()
	implType := reflect.TypeOf(s.impl)
	selfType := reflect.TypeOf(s)
	var methods []string
	for i := 0; i < ifaceType.NumMethod(); i++ {
		method := ifaceType.Method(i)
		if m, ok := implType.MethodByName(method.Name); !ok {
			continue
		} else if m.Type.NumIn() != method.Type.NumIn()+1 {
			continue
		} else if m.Type.NumOut() != method.Type.NumOut() {
			continue
		} else {
			match := true
			for j := 0; j < method.Type.NumIn(); j++ {
				if m.Type.In(j+1) != method.Type.In(j) {
					match = false
					break
				}
			}
			for j := 0; j < method.Type.NumOut(); j++ {
				if m.Type.Out(j) != method.Type.Out(j) {
					match = false
					break
				}
			}
			if !match {
				continue
			}
		}
		if _, ok := selfType.MethodByName(method.Name); !ok {
			continue
		}
		methods = append(methods, method.Name)
	}
	*reply = methods
	return nil
}

type Z_OnActivateArgs struct {
	APIMuxId uint32
}

type Z_OnActivateReturns struct {
	A error
}

func (g *hooksRPCClient) OnActivate() error {
	muxId := g.muxBroker.NextId()
	go g.muxBroker.AcceptAndServe(muxId, &apiRPCServer{
		impl: g.apiImpl,
	})

	_args := &Z_OnActivateArgs{
		APIMuxId: muxId,
	}
	_returns := &Z_OnActivateReturns{}

	if err := g.client.Call("Plugin.OnActivate", _args, _returns); err != nil {
		g.log.Error("RPC call to OnActivate plugin failed.", mlog.Err(err))
	}
	return _returns.A
}

func (s *hooksRPCServer) OnActivate(args *Z_OnActivateArgs, returns *Z_OnActivateReturns) error {
	connection, err := s.muxBroker.Dial(args.APIMuxId)
	if err != nil {
		return err
	}

	s.apiRPCClient = &apiRPCClient{
		client: rpc.NewClient(connection),
	}

	if mmplugin, ok := s.impl.(interface {
		SetAPI(api API)
		OnConfigurationChange() error
	}); !ok {
	} else {
		mmplugin.SetAPI(s.apiRPCClient)
		mmplugin.OnConfigurationChange()
	}

	// Capture output of standard logger because go-plugin
	// redirects it.
	log.SetOutput(os.Stderr)

	if hook, ok := s.impl.(interface {
		OnActivate() error
	}); ok {
		returns.A = hook.OnActivate()
	}
	return nil
}

type Z_LoadPluginConfigurationArgsArgs struct {
}

type Z_LoadPluginConfigurationArgsReturns struct {
	A []byte
}

func (g *apiRPCClient) LoadPluginConfiguration(dest interface{}) error {
	_args := &Z_LoadPluginConfigurationArgsArgs{}
	_returns := &Z_LoadPluginConfigurationArgsReturns{}
	if err := g.client.Call("Plugin.LoadPluginConfiguration", _args, _returns); err != nil {
		g.log.Error("RPC call to LoadPluginConfiguration API failed.", mlog.Err(err))
	}
	return json.Unmarshal(_returns.A, dest)
}

func (s *apiRPCServer) LoadPluginConfiguration(args *Z_LoadPluginConfigurationArgsArgs, returns *Z_LoadPluginConfigurationArgsReturns) error {
	var config interface{}
	if hook, ok := s.impl.(interface {
		LoadPluginConfiguration(dest interface{}) error
	}); ok {
		if err := hook.LoadPluginConfiguration(&config); err != nil {
			return err
		}
	}
	b, err := json.Marshal(config)
	if err != nil {
		return err
	}
	returns.A = b
	return nil
}

func init() {
	hookNameToId["ServeHTTP"] = ServeHTTPId
}

type Z_ServeHTTPArgs struct {
	ResponseWriterStream uint32
	Request              *http.Request
	Context              *Context
	RequestBodyStream    uint32
}

func (g *hooksRPCClient) ServeHTTP(c *Context, w http.ResponseWriter, r *http.Request) {
	if !g.implemented[ServeHTTPId] {
		http.NotFound(w, r)
		return
	}

	serveHTTPStreamId := g.muxBroker.NextId()
	go func() {
		connection, err := g.muxBroker.Accept(serveHTTPStreamId)
		if err != nil {
			g.log.Error("Plugin failed to ServeHTTP, muxBroker couldn't accept connection", mlog.Uint32("serve_http_stream_id", serveHTTPStreamId), mlog.Err(err))
			http.Error(w, "500 internal server error", http.StatusInternalServerError)
			return
		}
		defer connection.Close()

		rpcServer := rpc.NewServer()
		if err := rpcServer.RegisterName("Plugin", &httpResponseWriterRPCServer{w: w}); err != nil {
			g.log.Error("Plugin failed to ServeHTTP, coulden't register RPC name", mlog.Err(err))
			http.Error(w, "500 internal server error", http.StatusInternalServerError)
			return
		}
		rpcServer.ServeConn(connection)
	}()

	requestBodyStreamId := uint32(0)
	if r.Body != nil {
		requestBodyStreamId = g.muxBroker.NextId()
		go func() {
			bodyConnection, err := g.muxBroker.Accept(requestBodyStreamId)
			if err != nil {
				g.log.Error("Plugin failed to ServeHTTP, muxBroker couldn't Accept request body connecion", mlog.Err(err))
				http.Error(w, "500 internal server error", http.StatusInternalServerError)
				return
			}
			defer bodyConnection.Close()
			serveIOReader(r.Body, bodyConnection)
		}()
	}

	forwardedRequest := &http.Request{
		Method:     r.Method,
		URL:        r.URL,
		Proto:      r.Proto,
		ProtoMajor: r.ProtoMajor,
		ProtoMinor: r.ProtoMinor,
		Header:     r.Header,
		Host:       r.Host,
		RemoteAddr: r.RemoteAddr,
		RequestURI: r.RequestURI,
	}

	if err := g.client.Call("Plugin.ServeHTTP", Z_ServeHTTPArgs{
		Context:              c,
		ResponseWriterStream: serveHTTPStreamId,
		Request:              forwardedRequest,
		RequestBodyStream:    requestBodyStreamId,
	}, nil); err != nil {
		g.log.Error("Plugin failed to ServeHTTP, RPC call failed", mlog.Err(err))
		http.Error(w, "500 internal server error", http.StatusInternalServerError)
	}
	return
}

func (s *hooksRPCServer) ServeHTTP(args *Z_ServeHTTPArgs, returns *struct{}) error {
	connection, err := s.muxBroker.Dial(args.ResponseWriterStream)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR] Can't connect to remote response writer stream, error: %v", err.Error())
		return err
	}
	w := connectHTTPResponseWriter(connection)
	defer w.Close()

	r := args.Request
	if args.RequestBodyStream != 0 {
		connection, err := s.muxBroker.Dial(args.RequestBodyStream)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[ERROR] Can't connect to remote request body stream, error: %v", err.Error())
			return err
		}
		r.Body = connectIOReader(connection)
	} else {
		r.Body = ioutil.NopCloser(&bytes.Buffer{})
	}
	defer r.Body.Close()

	if hook, ok := s.impl.(interface {
		ServeHTTP(c *Context, w http.ResponseWriter, r *http.Request)
	}); ok {
		hook.ServeHTTP(args.Context, w, r)
	} else {
		http.NotFound(w, r)
	}

	return nil
}
