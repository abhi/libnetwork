package server

import (
	"os"

	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (c *CniService) getPods() (map[string]interface{}, error) {
	logrus.Infof("Received request to get pods")
	activeSandboxes := make(map[string]interface{})
	pods, err := c.k8ClientSet.CoreV1().Pods("").List(metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + os.Getenv("HOSTNAME"),
	})
	if err != nil {
		return nil, err
	}
	for _, pod := range pods.Items {
		if !pod.Spec.HostNetwork && pod.Status.Phase != "Pending" {
			meta, err := c.getCniMetadataFromStore(pod.Name, pod.Namespace)
			if err == nil && meta != nil {
				activeSandboxes[meta.SandboxID] = meta.SandboxMeta
			}
		}
	}
	logrus.Infof("Active Sandboxes: %+v", activeSandboxes)
	return activeSandboxes, nil
}
