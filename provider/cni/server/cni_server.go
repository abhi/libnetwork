package server

import (
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"

	"github.com/docker/libkv/store"
	"github.com/docker/libkv/store/boltdb"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"

	"github.com/docker/libnetwork/datastore"
	"github.com/docker/libnetwork/netutils"
	"github.com/docker/libnetwork/provider/cni/cniapi"
	cnistore "github.com/docker/libnetwork/provider/cni/store"
	"github.com/docker/libnetwork/types"
)

// CniService hold the cni service information
type CniService struct {
	listenPath string
	dnetConn   *netutils.HTTPConnection
	store      datastore.DataStore
}

// NewCniService returns a new cni service instance
func NewCniService(sock string, dnetIP string, dnetPort string) (*CniService, error) {
	dnetURL := dnetIP + ":" + dnetPort
	c := new(CniService)
	c.dnetConn = &netutils.HTTPConnection{Addr: dnetURL, Proto: "tcp"}
	c.listenPath = sock
	return c, nil
}

// InitCniService initializes the cni server
func (c *CniService) InitCniService(serverCloseChan chan struct{}) error {
	log.Infof("Starting CNI server")
	router := mux.NewRouter()
	t := router.Methods("POST").Subrouter()
	t.HandleFunc(cniapi.AddPodURL, MakeHTTPHandler(c, addPod))
	t.HandleFunc(cniapi.DelPodURL, MakeHTTPHandler(c, deletePod))
	syscall.Unlink(c.listenPath)
	boltdb.Register()
	store, err := localStore()
	if err != nil {
		return fmt.Errorf("failed to initialize local store: %v", err)
	}
	c.store = store
	go func() {
		l, err := net.ListenUnix("unix", &net.UnixAddr{Name: c.listenPath, Net: "unix"})
		if err != nil {
			panic(err)
		}
		log.Infof("Dnet CNI plugin listening on on %s", c.listenPath)
		http.Serve(l, router)
		l.Close()
		close(serverCloseChan)
	}()
	return nil
}

func localStore() (datastore.DataStore, error) {
	return datastore.NewDataStore(datastore.LocalScope, &datastore.ScopeCfg{
		Client: datastore.ScopeClientCfg{
			Provider: string(store.BOLTDB),
			Address:  "/var/run/libnetwork/cnidb.db",
			Config: &store.Config{
				Bucket:            "cni-dnet",
				ConnectionTimeout: 5 * time.Second,
			},
		},
	})
}

// GetStore returns store instance
func (c *CniService) GetStore() datastore.DataStore {
	return c.store
}

func (c *CniService) getCniMetadataFromStore(podName, podNamespace string) (*cnistore.CniMetadata, error) {
	store := c.GetStore()
	if store == nil {
		return nil, nil
	}
	cs := &cnistore.CniMetadata{PodName: podName, PodNamespace: podNamespace}
	if err := store.GetObject(datastore.Key(cs.Key()...), cs); err != nil {
		if err == datastore.ErrKeyNotFound {
			return nil, fmt.Errorf("failed to find cni metadata from store for %s pod %s namespace: %v",
				podName, podNamespace, err)
		}
		return nil, types.InternalErrorf("could not get pools config from store: %v", err)
	}
	return cs, nil
}

func (c *CniService) writeToStore(cs *cnistore.CniMetadata) error {
	store := c.GetStore()
	if store == nil {
		return nil
	}
	err := store.PutObjectAtomic(cs)
	if err == datastore.ErrKeyModified {
		return types.RetryErrorf("failed to perform atomic write (%v). retry might fix the error", err)
	}
	return err
}

func (c *CniService) deleteFromStore(cs *cnistore.CniMetadata) error {
	store := c.GetStore()
	if store == nil {
		return nil
	}
	return store.DeleteObjectAtomic(cs)
}
