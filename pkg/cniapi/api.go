package cniapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strings"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/plugins/pkg/ns"
	log "github.com/sirupsen/logrus"

	"github.com/docker/libnetwork/api"
)

const (
	AddPodUrl     = "/AddPod"
	DelPodUrl     = "/DelPod"
	GetActivePods = "/ActivePods"
	DnetCNISock   = "/var/run/cniserver.sock"
	PluginPath    = "/run/libnetwork"
)

type DnetCniClient struct {
	url        string
	httpClient *http.Client
}

type CniInfo struct {
	ContainerID string
	NetNS       string
	IfName      string
	NetConf     types.NetConf
	Metadata    map[string]string
}

func unixDial(proto, addr string) (conn net.Conn, err error) {
	sock := DnetCNISock
	return net.Dial("unix", sock)
}

func NewDnetCniClient() *DnetCniClient {
	c := new(DnetCniClient)
	c.url = "http://localhost"
	c.httpClient = &http.Client{
		Transport: &http.Transport{
			Dial: unixDial,
		},
	}
	return c
}

// SetupPod setups up the sandbox and endpoint for the infra container in a pod
func (l *DnetCniClient) SetupPod(args *skel.CmdArgs) (*current.Result, error) {
	var data current.Result
	log.Infof("Sending Setup Pod request %+v", args)
	podNetInfo, err := validatePodNetworkInfo(args)
	if err != nil {
		return nil, fmt.Errorf("failed to valid cni arguments, error: %v", err)
	}
	buf, err := json.Marshal(podNetInfo)
	if err != nil {
		return nil, err
	}
	body := bytes.NewBuffer(buf)
	url := l.url + AddPodUrl
	r, err := l.httpClient.Post(url, "application/json", body)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	switch {
	case r.StatusCode == int(404):
		return nil, fmt.Errorf("page not found")

	case r.StatusCode == int(403):
		return nil, fmt.Errorf("access denied")

	case r.StatusCode == int(500):
		info, err := ioutil.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		err = json.Unmarshal(info, &data)
		if err != nil {
			return nil, err
		}
		return &data, fmt.Errorf("Internal Server Error")

	case r.StatusCode != int(200):
		log.Errorf("POST Status '%s' status code %d \n", r.Status, r.StatusCode)
		return nil, fmt.Errorf("%s", r.Status)
	}

	response, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}

	err = json.Unmarshal(response, &data)
	if err != nil {
		return nil, err
	}
	return &data, nil
}

// TearDownPod tears the sandbox and endpoint created for the infra
// container in the pod.
func (l *DnetCniClient) TearDownPod(args *skel.CmdArgs) error {
	log.Infof("Sending Teardown Pod request %+v", args)
	podNetInfo, err := validatePodNetworkInfo(args)
	if err != nil {
		return fmt.Errorf("failed to validate cni arguments, error: %v", err)
	}
	buf, err := json.Marshal(podNetInfo)
	if err != nil {
		return err
	}
	body := bytes.NewBuffer(buf)
	url := l.url + DelPodUrl
	r, err := l.httpClient.Post(url, "application/json", body)
	defer r.Body.Close()
	if err != nil {
		return err
	}
	return nil
}

// GetActivePods returns a list of active pods and their sandboxIDs
func (l *DnetCniClient) GetActiveSandboxes() (map[string]api.SandboxMetadata, error) {
	log.Infof("Requesting for for active sandboxes")
	var sandboxes map[string]api.SandboxMetadata
	url := l.url + GetActivePods
	r, err := l.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed during http get :%v", err)
	}
	defer r.Body.Close()
	response, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(response, &sandboxes)
	if err != nil {
		return nil, fmt.Errorf("failed to decode http response: %v", err)
	}

	return sandboxes, nil
}

func validatePodNetworkInfo(args *skel.CmdArgs) (*CniInfo, error) {
	rt := new(CniInfo)
	if args.ContainerID == "" {
		return nil, fmt.Errorf("containerID empty")
	}
	rt.ContainerID = args.ContainerID
	if args.Netns == "" {
		return nil, fmt.Errorf("network namespace not present")
	}
	_, err := ns.GetNS(args.Netns)
	if err != nil {
		return nil, err
	}
	rt.NetNS = args.Netns
	if args.IfName == "" {
		rt.IfName = "eth1"
	} else {
		rt.IfName = args.IfName
	}
	var netConf struct {
		types.NetConf
	}
	if err := json.Unmarshal(args.StdinData, &netConf); err != nil {
		return nil, fmt.Errorf("failed to unmarshal network configuration :%v", err)
	}
	rt.NetConf = netConf.NetConf
	if args.Args != "" {
		rt.Metadata = getMetadataFromArgs(args.Args)
	}
	return rt, nil
}

func getMetadataFromArgs(args string) map[string]string {
	m := make(map[string]string)
	for _, a := range strings.Split(args, ";") {
		if strings.Contains(a, "=") {
			kvPair := strings.Split(a, "=")
			m[strings.TrimSpace(kvPair[0])] = strings.TrimSpace(kvPair[1])
		} else {
			m[a] = ""
		}
	}
	return m
}
