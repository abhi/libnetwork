package server

import (
	"encoding/json"
	"fmt"
	log "github.com/sirupsen/logrus"
	"io/ioutil"
	"net"
	"net/http"
	"reflect"
	"strings"

	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/docker/libnetwork/client"
	"github.com/docker/libnetwork/netutils"
	"github.com/docker/libnetwork/provider/cni/cniapi"
	cnistore "github.com/docker/libnetwork/provider/cni/store"
)

func addPod(w http.ResponseWriter, r *http.Request, c *CniService, vars map[string]string) (_ interface{}, retErr error) {
	cniInfo := cniapi.CniInfo{}
	var result current.Result

	content, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Errorf("Failed to read request: %v", err)
		return cniInfo, err
	}

	if err := json.Unmarshal(content, &cniInfo); err != nil {
		return cniInfo, err
	}

	log.Infof("Received add pod request %+v", cniInfo)
	// Create a Sandbox
	sbConfig, sbID, err := c.createSandbox(cniInfo.ContainerID)
	if err != nil {
		return cniInfo, fmt.Errorf("failed to create sandbox for %q: %v", cniInfo.ContainerID, err)
	}
	defer func() {
		if retErr != nil {
			if err := c.deleteSandbox(sbID); err != nil {
				log.Warnf("failed to delete sandbox %v on setup pod failure , error:%v", sbID, err)
			}
		}
	}()
	// Create an Endpoint
	ep, err := c.createEndpoint(cniInfo.ContainerID, cniInfo.NetConf)
	if err != nil {
		return nil, fmt.Errorf("failed to create endpoint for %q: %v", cniInfo.ContainerID, err)
	}
	defer func() {
		if retErr != nil {
			if err := c.deleteEndpoint(ep.ID); err != nil {
				log.Warnf("failed to delete endpoint %v on setup pod failure , error:%v", ep.ID, err)
			}
		}
	}()
	// Attach endpoint to the sandbox
	if err = c.endpointJoin(sbID, ep.ID, cniInfo.NetNS); err != nil {
		return nil, fmt.Errorf("failed to attach endpoint to sandbox for container:%q,sandbox:%q,endpoint:%q, error:%v", cniInfo.ContainerID, sbID, ep.ID, err)
	}
	defer func() {
		if retErr != nil {
			if err = c.endpointLeave(sbID, ep.ID); err != nil {
				log.Warnf("failed to detach endpoint %q from sandbox %q , err:%v", ep.ID, sbID, err)
			}
		}
	}()
	cs := &cnistore.CniMetadata{
		PodName:          cniInfo.Metadata["K8S_POD_NAME"],
		PodNamespace:     cniInfo.Metadata["K8S_POD_NAMESPACE"],
		InfraContainerID: cniInfo.Metadata["K8S_POD_INFRA_CONTAINER_ID"],
		SandboxID:        sbID,
		EndpointID:       ep.ID,
		SandboxMeta:      cnistore.CopySandboxMetadata(sbConfig, cniInfo.NetNS),
	}
	if err := c.writeToStore(cs); err != nil {
		return nil, fmt.Errorf("failed to write cni metadata to store: %v", err)
	}
	result.Interfaces = append(result.Interfaces, &current.Interface{Name: "eth1", Mac: ep.MacAddress.String()})
	if !reflect.DeepEqual(ep.Address, (net.IPNet{})) {
		result.IPs = append(result.IPs, &current.IPConfig{
			Version: "4",
			Address: ep.Address,
			Gateway: ep.Gateway,
		})
	}
	if !reflect.DeepEqual(ep.AddressIPv6, (net.IPNet{})) {
		result.IPs = append(result.IPs, &current.IPConfig{
			Version: "6",
			Address: ep.AddressIPv6,
			Gateway: ep.GatewayIPv6,
		})
	}
	//TODO : Point IPs to the interface index

	return result, err

}

func (c *CniService) createSandbox(ContainerID string) (client.SandboxCreate, string, error) {
	sc := client.SandboxCreate{ContainerID: ContainerID, UseExternalKey: true}
	obj, _, err := netutils.ReadBody(c.dnetConn.HTTPCall("POST", "/sandboxes", sc, nil))
	if err != nil {
		return client.SandboxCreate{}, "", err
	}

	var replyID string
	err = json.Unmarshal(obj, &replyID)
	if err != nil {
		return client.SandboxCreate{}, "", err
	}
	return sc, replyID, nil
}

func (c *CniService) createEndpoint(ContainerID string, netConfig cniapi.NetworkConf) (client.EndpointInfo, error) {
	var ep client.EndpointInfo
	// Create network if it doesnt exist. Need to handle refcount to delete
	// network on last pod delete.
	if !c.networkExists(netConfig.Name) {
		if err := c.createNetwork(netConfig); err != nil && !strings.Contains(err.Error(), "already exists") {
			return ep, err
		}
	}

	sc := client.ServiceCreate{Name: ContainerID, Network: netConfig.Name, DisableResolution: true}
	obj, _, err := netutils.ReadBody(c.dnetConn.HTTPCall("POST", "/services", sc, nil))
	if err != nil {
		return ep, err
	}
	err = json.Unmarshal(obj, &ep)
	return ep, err
}

func (c *CniService) endpointJoin(sandboxID, endpointID, netns string) (retErr error) {
	nc := client.ServiceAttach{SandboxID: sandboxID, SandboxKey: netns}
	_, _, err := netutils.ReadBody(c.dnetConn.HTTPCall("POST", "/services/"+endpointID+"/backend", nc, nil))
	return err
}

func (c *CniService) networkExists(networkID string) bool {
	obj, statusCode, err := netutils.ReadBody(c.dnetConn.HTTPCall("GET", "/networks?partial-id="+networkID, nil, nil))
	if err != nil {
		log.Debugf("%s network does not exist:%v \n", networkID, err)
		return false
	}
	if statusCode != http.StatusOK {
		log.Debugf("%s network does not exist \n", networkID)
		return false
	}
	var list []*client.NetworkResource
	err = json.Unmarshal(obj, &list)
	if err != nil {
		return false
	}
	return (len(list) != 0)
}

// createNetwork is a very simple utility to create a default network
// if not present.
//TODO: Need to watch out for parallel createnetwork calls on multiple nodes
func (c *CniService) createNetwork(netConf cniapi.NetworkConf) error {
	log.Infof("Creating network %+v \n", netConf)
	driverOpts := make(map[string]string)
	driverOpts["hostaccess"] = ""
	nc := client.NetworkCreate{Name: netConf.Name, ID: netConf.Name, NetworkType: getNetworkType(netConf.Type),
		DriverOpts: driverOpts}
	if ipam := netConf.IPAM; ipam != nil {
		cfg := client.IPAMConf{}
		if ipam.PreferredPool != "" {
			cfg.PreferredPool = ipam.PreferredPool
		}
		if ipam.SubPool != "" {
			cfg.SubPool = ipam.SubPool
		}
		if ipam.Gateway != "" {
			cfg.Gateway = ipam.Gateway
		}
		nc.IPv4Conf = []client.IPAMConf{cfg}
	}
	obj, _, err := netutils.ReadBody(c.dnetConn.HTTPCall("POST", "/networks", nc, nil))
	if err != nil {
		return err
	}
	var replyID string
	err = json.Unmarshal(obj, &replyID)
	if err != nil {
		return err
	}
	fmt.Printf("Network creation succeeded: %v", replyID)
	return nil
}

func getNetworkType(networkType string) string {
	switch networkType {
	case "dnet-overlay":
		return "overlay"
	case "dnet-bridge":
		return "bridge"
	case "dnet-ipvlan":
		return "ipvlan"
	case "dnet-macvlan":
		return "macvlan"
	default:
		return "overlay"
	}
}
