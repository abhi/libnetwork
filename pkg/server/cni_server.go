package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/docker/libkv/store"
	"github.com/docker/libkv/store/boltdb"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/docker/libnetwork/datastore"
	"github.com/docker/libnetwork/netutils"
	"github.com/docker/libnetwork/pkg/cniapi"
	cnistore "github.com/docker/libnetwork/pkg/store"
	"github.com/docker/libnetwork/types"
)

const (
	CniServicePort = 9005
)

type CniService struct {
	//TODO k8sClient *APIClient

	listenPath      string
	dnetConn        *netutils.HttpConnection
	sandboxIDStore  map[string]string // containerID to sandboxID mapping
	endpointIDStore map[string]string // containerID to endpointID mapping
	store           datastore.DataStore
	k8ClientSet     *kubernetes.Clientset
}

func NewCniService(sock string, dnetIP string, dnetPort string) (*CniService, error) {
	dnetUrl := dnetIP + ":" + dnetPort
	c := new(CniService)
	c.dnetConn = &netutils.HttpConnection{Addr: dnetUrl, Proto: "tcp"}
	c.listenPath = sock
	c.sandboxIDStore = make(map[string]string)
	c.endpointIDStore = make(map[string]string)
	return c, nil
}

// InitCniService initializes the cni server
func (c *CniService) InitCniService(serverCloseChan chan struct{}) error {
	log.Infof("Starting CNI server")
	// Create http handlers for add and delete pod
	router := mux.NewRouter()
	t := router.Methods("POST").Subrouter()
	t.HandleFunc(cniapi.AddPodUrl, MakeHTTPHandler(c, addPod))
	t.HandleFunc(cniapi.DelPodUrl, MakeHTTPHandler(c, deletePod))

	t = router.Methods("GET").Subrouter()
	t.HandleFunc(cniapi.GetActivePods, func(w http.ResponseWriter, r *http.Request) {
		resp, err := c.getPods()
		if err != nil {
			fmt.Printf("error is %v \n", err)
			http.Error(w, "failed to fetch active sandboxes1", http.StatusInternalServerError)
			return
		}
		fmt.Printf("Response: %v \n", resp)
		jsonData, err := json.Marshal(resp)
		if err != nil {
			http.Error(w, "failed to fetch active sandboxes2", http.StatusInternalServerError)
		}
		fmt.Printf("JSON DATA:%v \n", jsonData)
		w.Header().Set("Content-Type", "application/json")
		w.Write(jsonData)
	})

	syscall.Unlink(c.listenPath)
	os.MkdirAll(cniapi.PluginPath, 0700)
	boltdb.Register()
	log.Infof("Setting up local store")
	store, err := localStore()
	if err != nil {
		fmt.Errorf("failed to initialize local store: %v", err)
	}
	c.store = store
	// creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("failed to initialize in-cluster config: %v", err.Error())
	}
	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create api server client: %v", err)
	}
	c.k8ClientSet = clientset

	go func() {
		l, err := net.ListenUnix("unix", &net.UnixAddr{Name: c.listenPath, Net: "unix"})
		if err != nil {
			panic(err)
		}
		log.Infof("Libnetwork CNI plugin listening on on %s", c.listenPath)
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

func (c *CniService) GetStore() datastore.DataStore {
	return c.store
}

func (c *CniService) getCniMetadataFromStore(podName, podNamespace string) (*cnistore.CniStore, error) {
	store := c.GetStore()
	if store == nil {
		fmt.Printf("store empty \n")
		return nil, nil
	}
	cs := &cnistore.CniStore{PodName: podName, PodNamespace: podNamespace}
	fmt.Printf("Read from store key: %v\n", cs.Key())
	if err := store.GetObject(datastore.Key(cs.Key()...), cs); err != nil {
		if err == datastore.ErrKeyNotFound {
			fmt.Printf("Key not found !!!! \n")
			return nil, nil
		}
		return nil, types.InternalErrorf("could not get pools config from store: %v", err)
	}
	return cs, nil
}

func (c *CniService) writeToStore(cs *cnistore.CniStore) error {
	store := c.GetStore()
	if store == nil {
		fmt.Printf("Writing to store. Store Empty \n")
		return nil
	}
	fmt.Printf("Writing to store key: %v\n", cs.Key())
	err := store.PutObjectAtomic(cs)
	if err == datastore.ErrKeyModified {
		return types.RetryErrorf("failed to perform atomic write (%v). retry might fix the error", err)
	}
	return err
}

func (c *CniService) deleteFromStore(cs *cnistore.CniStore) error {
	store := c.GetStore()
	if store == nil {
		return nil
	}
	return store.DeleteObjectAtomic(cs)
}
