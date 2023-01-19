package httputil

import (
	"context"
	"crypto/tls"
	"git.padmadigital.id/potato/go-logutil"
	"log"
	"net/http"
	"sync"
)

// The ServerManager controls HTTP and HTTPS servers that are deployed and listening to connections.
// It tracks and waits for all servers to shutdown (by invoking Wait), and can shuts down all servers at once by calling
// Shutdown. When one server cannot be launched, all the remaining servers are shut down.
//
// To use the ServerManager, simply create the manager and spawn HTTP/HTTPS servers using SpawnHttp and SpawnHttps
// methods and you can expect the manager to control them for you. Right now there is no support for shutting down
// individual server.
type ServerManager struct {
	servers     []*http.Server // A slice of all HTTP servers run using ListenAndServe.
	tlsServers  []*http.Server // A slice of all TLS servers run using ListenAndServeTLS.
	stopChannel chan struct{}  // A channel for the ServerManager to tell each server to stop.
	stopOnce    *sync.Once
	wg          *sync.WaitGroup // WaitGroup for waiting for all servers to exit.
	Logger      logutil.Logger  // An instance of logutil.Logger for the manager to log to.
	lastErr     error
	// TlsConfig defines a custom TLS config to be passed to TLS servers when started.
	// It cannot be changed in runtime, so servers have to be restarted again.
	TlsConfig *tls.Config
}

// NewServerManager creates a new ServerManager with the default logger.
// By default, it uses StdLogger with "ServerManager" as prefix. You can change it by changing the Logger.
func NewServerManager() *ServerManager {
	manager := &ServerManager{
		servers:     make([]*http.Server, 1),
		tlsServers:  make([]*http.Server, 0),
		stopChannel: make(chan struct{}),
		stopOnce:    &sync.Once{},
		wg:          &sync.WaitGroup{},
		Logger:      logutil.NewStdLogger(true, "ServerManager"),
	}

	return manager
}

// Create a new server.
func (manager *ServerManager) createServer(listenAddress string, handler http.Handler, onShutdown func()) (server *http.Server) {
	server = &http.Server{
		Addr:      listenAddress,
		Handler:   handler,
		ErrorLog:  log.New(&logutil.StdLogAdapter{Logger: manager.Logger}, "", 0),
		TLSConfig: manager.TlsConfig,
	}

	server.RegisterOnShutdown(onShutdown)
	return
}

// waitForStopChannel waits for the stopChannel to be closed, then shut down the given server.
func (manager *ServerManager) waitForStopChannel(server *http.Server) {
	for {
		select {
		case <-manager.stopChannel:
			_ = server.Shutdown(context.Background())
			return
		}
	}
}

// SpawnHttp spawns a single HTTP server using listenAddress and router as the primary Handler.
// When the server encounters and error other than http.ErrServerClosed, it will tell the ServerManager to shut down.
// http.ErrServerClosed is emitted when the server is shut down, which means it is not a real error.
func (manager *ServerManager) SpawnHttp(listenAddress string, handler http.Handler) {
	server := manager.createServer(listenAddress, handler, func() {
		manager.wg.Done()
		manager.Logger.Tracef("server on %s reached shut down", listenAddress)
	})
	manager.servers = append(manager.servers, server)

	manager.wg.Add(1)
	go func() {
		manager.Logger.Infof("spawning HTTP server on %s", listenAddress)
		err := server.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			manager.lastErr = err
			manager.Logger.Error(err.Error())
			manager.Shutdown()
		}
	}()

	go manager.waitForStopChannel(server)
}

// BatchSpawnHttp spawns a single HTTP TLS server using listenAddress and router as the primary Handler.
// See also SpawnHttp.
func (manager *ServerManager) BatchSpawnHttp(listenAddresses []string, handler http.Handler) {
	for _, addr := range listenAddresses {
		manager.SpawnHttp(addr, handler)
	}
}

// SpawnHttpTls spawns multiple HTTP servers at once on each listenAddress.
// See also SpawnHttp.
func (manager *ServerManager) SpawnHttpTls(listenAddress string, handler http.Handler, certFile, keyFile string) {
	server := manager.createServer(listenAddress, handler, func() {
		manager.wg.Done()
		manager.Logger.Tracef("server on %s reached shut down", listenAddress)
	})
	manager.tlsServers = append(manager.tlsServers, server)

	manager.wg.Add(1)
	go func() {
		manager.Logger.Infof("spawning HTTP TLS server on %s", listenAddress)
		err := server.ListenAndServeTLS(certFile, keyFile)
		if err != nil && err != http.ErrServerClosed {
			manager.lastErr = err
			manager.Logger.Error(err.Error())
			manager.Shutdown()
		}
	}()

	go manager.waitForStopChannel(server)
}

// BatchSpawnHttps spawns multiple HTTP TLS servers at once on each listenAddress.
// See also SpawnHttp.
func (manager *ServerManager) BatchSpawnHttps(listenAddresses []string, handler http.Handler, certFile, keyFile string) {
	for _, addr := range listenAddresses {
		manager.SpawnHttpTls(addr, handler, certFile, keyFile)
	}
}

// Err returns the last error that is encountered when spawning servers.
// Because spawning servers is an async process, Err() might not return an error immediately
// after the server is started. You might want to wait for a few seconds after spawning before checking on
// Err before stopping the whole server manager.
//
// Example error: address already in use.
func (manager *ServerManager) Err() error {
	return manager.lastErr
}

// Shutdown sends a signal to ServerManager to shut all the servers down.
func (manager *ServerManager) Shutdown() {
	manager.Logger.Trace("sending shutdown signal to ServerManager")
	manager.stopOnce.Do(func() {
		close(manager.stopChannel)
	})
}

// Wait for all servers to finish all operations.
func (manager *ServerManager) Wait() {
	manager.Logger.Trace("now waiting for all HTTP(S) servers to finish")
	manager.wg.Wait()
}
