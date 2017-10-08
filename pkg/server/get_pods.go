package server

import (
	"fmt"
	"os"

	//k8errors "k8s.io/apimachinery/pkg/api/errors"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (c *CniService) getPods() (map[string]interface{}, error) {
	logrus.Infof("Received request to get pod")
	activeSandboxes := make(map[string]interface{})
	pods, err := c.k8ClientSet.CoreV1().Pods("").List(metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + os.Getenv("HOSTNAME"),
	})
	if err != nil {
		return nil, err
	}
	for _, pod := range pods.Items {
		if !pod.Spec.HostNetwork && pod.Status.Phase != "Pending" {
			fmt.Printf("POD:{%+v} \n", pod)
			meta, err := c.getCniMetadataFromStore(pod.Name, pod.Namespace)
			if err == nil && meta != nil {
				activeSandboxes[meta.SandboxID] = meta.SandboxConfig
			}
		}
	}
	fmt.Printf("Active Sandboxes:%d,  %+v , err:%v  \n", len(activeSandboxes), activeSandboxes, err)
	return activeSandboxes, nil
}

/*
func getSandboxOptions(sc client.SandboxCreate) []libnetwork.SandboxOption {
	var sbOptions []libnetwork.SandboxOption
	if sc.UseExternalKey {
		sbOptions = append(sbOptions, libnetwork.OptionUseExternalKey())
	}
	return sbOptions
}
*/
