package server

import (
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"time"

	log "github.com/Sirupsen/logrus"
	broker "github.com/docker/infrakit/pkg/broker/server"
	rpc_server "github.com/docker/infrakit/pkg/rpc"
	"github.com/docker/infrakit/pkg/spi"
	"github.com/docker/infrakit/pkg/spi/event"
	"github.com/docker/infrakit/pkg/types"
	"github.com/gorilla/mux"
	"github.com/gorilla/rpc/v2"
	"github.com/gorilla/rpc/v2/json2"
	"gopkg.in/tylerb/graceful.v1"
)

// Stoppable support proactive stopping, and blocking until stopped.
type Stoppable interface {
	Stop()
	AwaitStopped()
	Wait() <-chan struct{}
}

type stoppableServer struct {
	server *graceful.Server
}

func (s *stoppableServer) Stop() {
	s.server.Stop(10 * time.Second)
}

func (s *stoppableServer) Wait() <-chan struct{} {
	return s.server.StopChan()
}

func (s *stoppableServer) AwaitStopped() {
	<-s.server.StopChan()
}

type loggingHandler struct {
	handler http.Handler
}

func (h loggingHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	requestData, err := httputil.DumpRequest(req, true)
	if err == nil {
		log.Debugf("Received request %s", string(requestData))
	} else {
		log.Error(err)
	}

	recorder := httptest.NewRecorder()

	h.handler.ServeHTTP(recorder, req)

	responseData, err := httputil.DumpResponse(recorder.Result(), true)
	if err == nil {
		log.Debugf("Sending response %s", string(responseData))
	} else {
		log.Error(err)
	}

	w.WriteHeader(recorder.Code)
	recorder.Body.WriteTo(w)
}

// A VersionedInterface identifies which Interfaces a plugin supports.
type VersionedInterface interface {
	// ImplementedInterface returns the interface being provided.
	ImplementedInterface() spi.InterfaceSpec
	// Types returns the types in this category/ kind
	Types() []string
}

// StartListenerAtPath starts an HTTP server listening on tcp port with discovery entry at specified path.
// Returns a Stoppable that can be used to stop or block on the server.
func StartListenerAtPath(listen []string, discoverPath string,
	receiver VersionedInterface, more ...VersionedInterface) (Stoppable, error) {
	return startAtPath(listen, discoverPath, receiver, more...)
}

// StartPluginAtPath starts an HTTP server listening on a unix socket at the specified path.
// Returns a Stoppable that can be used to stop or block on the server.
func StartPluginAtPath(socketPath string, receiver VersionedInterface, more ...VersionedInterface) (Stoppable, error) {
	return startAtPath(nil, socketPath, receiver, more...)
}

func startAtPath(listen []string, discoverPath string,
	receiver VersionedInterface, more ...VersionedInterface) (Stoppable, error) {

	server := rpc.NewServer()
	server.RegisterCodec(json2.NewCodec(), "application/json")

	targets := append([]VersionedInterface{receiver}, more...)

	interfaces := map[spi.InterfaceSpec][]string{}
	for _, t := range targets {

		interfaces[t.ImplementedInterface()] = t.Types()

		if err := server.RegisterService(t, ""); err != nil {
			return nil, err
		}
	}

	// handshake service that can exchange interface versions with client
	if err := server.RegisterService(rpc_server.Handshake(interfaces), ""); err != nil {
		return nil, err
	}

	// events handler
	events := broker.NewBroker()

	// wire up the publish event source channel to the plugin implementations
	for _, t := range targets {

		pub, is := t.(event.Publisher)
		if !is {
			continue
		}

		// We give one channel per source to provide some isolation.  This we won't have the
		// whole event bus stop just because one plugin closes the channel.
		eventChan := make(chan *event.Event)
		pub.PublishOn(eventChan)
		go func() {
			for {
				event, ok := <-eventChan
				if !ok {
					return
				}
				events.Publish(event.Topic.String(), event, 1*time.Second)
			}
		}()
	}

	// info handler
	info, err := NewPluginInfo(receiver)
	if err != nil {
		return nil, err
	}

	httpLog := log.New()
	httpLog.Level = log.GetLevel()

	router := mux.NewRouter()
	router.HandleFunc(rpc_server.URLAPI, info.ShowAPI)
	router.HandleFunc(rpc_server.URLFunctions, info.ShowTemplateFunctions)

	intercept := broker.Interceptor{
		Pre: func(topic string, headers map[string][]string) error {
			for _, target := range targets {
				if v, is := target.(event.Validator); is {
					if err := v.Validate(types.PathFromString(topic)); err == nil {
						return nil
					}
				}
			}
			return broker.ErrInvalidTopic(topic)
		},
		Do: events.ServeHTTP,
		Post: func(topic string) {
			log.Infoln("Client left", topic)
		},
	}
	router.HandleFunc(rpc_server.URLEventsPrefix, intercept.ServeHTTP)

	logger := loggingHandler{handler: server}
	router.Handle("/", logger)

	gracefulServer := graceful.Server{
		Timeout: 10 * time.Second,
	}

	var listener net.Listener

	if len(listen) > 0 {
		gracefulServer.Server = &http.Server{
			Addr:    listen[0],
			Handler: router,
		}
		l, err := net.Listen("tcp", listen[0])
		if err != nil {
			return nil, err
		}
		listener = l

		advertise := listen[0]
		if len(listen) > 1 {
			advertise = listen[1]
		}
		if err := ioutil.WriteFile(discoverPath, []byte(fmt.Sprintf("tcp://%s", advertise)), 0644); err != nil {
			return nil, err
		}

		log.Infof("Listening at: %s, discoverable at %s", listen, discoverPath)

	} else {
		gracefulServer.Server = &http.Server{
			Addr:    fmt.Sprintf("unix://%s", discoverPath),
			Handler: router,
		}
		l, err := net.Listen("unix", discoverPath)
		if err != nil {
			return nil, err
		}
		listener = l
		log.Infof("Listening at: %s", discoverPath)

	}

	go func() {
		err := gracefulServer.Serve(listener)
		if err != nil {
			log.Warn(err)
		}
		events.Stop()
		if len(listen) > 0 {
			os.Remove(discoverPath)
		}
	}()

	return &stoppableServer{server: &gracefulServer}, nil
}
