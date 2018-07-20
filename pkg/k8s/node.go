// Copyright 2016-2017 Authors of Cilium
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package k8s

import (
	"errors"
	"net"
	"time"

	"github.com/cilium/cilium/pkg/annotation"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/node"

	"github.com/sirupsen/logrus"
	"k8s.io/api/core/v1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var (
	// ErrNilNode is returned when the Kubernetes API server has returned a nil node
	ErrNilNode = errors.New("API server returned nil node")
)

// ParseNode parses a kubernetes node to a cilium node
func ParseNode(k8sNode *v1.Node) *node.Node {
	scopedLog := log.WithFields(logrus.Fields{
		logfields.NodeName:  k8sNode.Name,
		logfields.K8sNodeID: k8sNode.UID,
	})
	addrs := []node.Address{}
	for _, addr := range k8sNode.Status.Addresses {
		// We only care about this address types,
		// we ignore all other types.
		switch addr.Type {
		case v1.NodeInternalIP, v1.NodeExternalIP:
		default:
			continue
		}
		ip := net.ParseIP(addr.Address)
		if ip == nil {
			scopedLog.WithFields(logrus.Fields{
				logfields.IPAddr: addr.Address,
				"type":           addr.Type,
			}).Warn("Ignoring invalid node IP")
			continue
		}
		na := node.Address{
			AddressType: addr.Type,
			IP:          ip,
		}
		addrs = append(addrs, na)
	}

	node := &node.Node{
		Name:        k8sNode.Name,
		IPAddresses: addrs,
	}

	if len(k8sNode.Spec.PodCIDR) != 0 {
		if _, cidr, err := net.ParseCIDR(k8sNode.Spec.PodCIDR); err != nil {
			scopedLog.WithError(err).WithField(logfields.V4Prefix, k8sNode.Spec.PodCIDR).Warn("Invalid PodCIDR value for node")
		} else {
			if cidr.IP.To4() != nil {
				node.IPv4AllocCIDR = cidr
			} else {
				node.IPv6AllocCIDR = cidr
			}
		}
	}
	// Spec.PodCIDR takes precedence since it's
	// the CIDR assigned by k8s controller manager
	// In case it's invalid or empty then we fall back to our annotations.
	if node.IPv4AllocCIDR == nil {
		if ipv4CIDR, ok := k8sNode.Annotations[annotation.V4CIDRName]; !ok {
			scopedLog.Debug("Empty IPv4 CIDR annotation in node")
		} else {
			_, cidr, err := net.ParseCIDR(ipv4CIDR)
			if err != nil {
				scopedLog.WithError(err).WithField(logfields.V4Prefix, ipv4CIDR).Error("BUG, invalid IPv4 annotation CIDR in node")
			} else {
				node.IPv4AllocCIDR = cidr
			}
		}
	}

	if node.IPv6AllocCIDR == nil {
		if ipv6CIDR, ok := k8sNode.Annotations[annotation.V6CIDRName]; !ok {
			scopedLog.Debug("Empty IPv6 CIDR annotation in node")
		} else {
			_, cidr, err := net.ParseCIDR(ipv6CIDR)
			if err != nil {
				scopedLog.WithError(err).WithField(logfields.V6Prefix, ipv6CIDR).Error("BUG, invalid IPv6 annotation CIDR in node")
			} else {
				node.IPv6AllocCIDR = cidr
			}
		}
	}

	if node.IPv4HealthIP == nil {
		if healthIP, ok := k8sNode.Annotations[annotation.V4HealthName]; !ok {
			scopedLog.Debug("Empty IPv4 health endpoint annotation in node")
		} else if ip := net.ParseIP(healthIP); ip == nil {
			scopedLog.WithField(logfields.V4HealthIP, healthIP).Error("BUG, invalid IPv4 health endpoint annotation in node")
		} else {
			node.IPv4HealthIP = ip
		}
	}

	if node.IPv6HealthIP == nil {
		if healthIP, ok := k8sNode.Annotations[annotation.V6HealthName]; !ok {
			scopedLog.Debug("Empty IPv6 health endpoint annotation in node")
		} else if ip := net.ParseIP(healthIP); ip == nil {
			scopedLog.WithField(logfields.V6HealthIP, healthIP).Error("BUG, invalid IPv6 health endpoint annotation in node")
		} else {
			node.IPv6HealthIP = ip
		}
	}

	return node
}

// GetNode returns the kubernetes nodeName's node information from the
// kubernetes api server
func GetNode(c kubernetes.Interface, nodeName string) (*v1.Node, error) {
	// Try to retrieve node's cidr and addresses from k8s's configuration
	return c.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
}

func updateNodeAnnotation(c kubernetes.Interface, node *v1.Node, v4CIDR, v6CIDR *net.IPNet, v4HealthIP, v6HealthIP, v4CiliumHostIP net.IP) (*v1.Node, error) {
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}

	if v4CIDR != nil {
		node.Annotations[annotation.V4CIDRName] = v4CIDR.String()
	}
	if v6CIDR != nil {
		node.Annotations[annotation.V6CIDRName] = v6CIDR.String()
	}

	if v4HealthIP != nil {
		node.Annotations[annotation.V4HealthName] = v4HealthIP.String()
	}
	if v6HealthIP != nil {
		node.Annotations[annotation.V6HealthName] = v6HealthIP.String()
	}

	if v4CiliumHostIP != nil {
		node.Annotations[annotation.CiliumHostIP] = v4CiliumHostIP.String()
	}

	node, err := c.CoreV1().Nodes().Update(node)
	if err != nil {
		return nil, err
	}

	if node == nil {
		return nil, ErrNilNode
	}

	return node, nil
}

// AnnotateNode writes v4 and v6 CIDRs and health IPs in the given k8s node name.
// In case of failure while updating the node, this function while spawn a go
// routine to retry the node update indefinitely.
func AnnotateNode(c kubernetes.Interface, nodeName string, v4CIDR, v6CIDR *net.IPNet, v4HealthIP, v6HealthIP, v4CiliumHostIP net.IP) error {
	scopedLog := log.WithFields(logrus.Fields{
		logfields.NodeName:       nodeName,
		logfields.V4Prefix:       v4CIDR,
		logfields.V6Prefix:       v6CIDR,
		logfields.V4HealthIP:     v4HealthIP,
		logfields.V6HealthIP:     v6HealthIP,
		logfields.V4CiliumHostIP: v4CiliumHostIP,
	})
	scopedLog.Debug("Updating node annotations with node CIDRs")

	go func(c kubernetes.Interface, nodeName string, v4CIDR, v6CIDR *net.IPNet, v4HealthIP, v6HealthIP, v4CiliumHostIP net.IP) {
		var node *v1.Node
		var err error

		for n := 1; n <= maxUpdateRetries; n++ {
			node, err = GetNode(c, nodeName)
			switch {
			case err == nil:
				_, err = updateNodeAnnotation(c, node, v4CIDR, v6CIDR, v4HealthIP, v6HealthIP, v4CiliumHostIP)
			case k8sErrors.IsNotFound(err):
				err = ErrNilNode
			}

			switch {
			case err == nil:
				return
			case k8sErrors.IsConflict(err):
				scopedLog.WithFields(logrus.Fields{
					fieldRetry:    n,
					fieldMaxRetry: maxUpdateRetries,
				}).WithError(err).Debugf("Unable to update node resource with annotation")
			default:
				scopedLog.WithFields(logrus.Fields{
					fieldRetry:    n,
					fieldMaxRetry: maxUpdateRetries,
				}).WithError(err).Warn("Unable to update node resource with annotation")
			}

			time.Sleep(time.Duration(n) * time.Second)
		}
	}(c, nodeName, v4CIDR, v6CIDR, v4HealthIP, v6HealthIP, v4CiliumHostIP)

	return nil
}
