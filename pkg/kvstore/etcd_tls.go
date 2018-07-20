// Copyright 2018 Authors of Cilium
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

package kvstore

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"

	k8sclient "github.com/cilium/cilium/pkg/k8s/client"

	"github.com/coreos/etcd-operator/pkg/util/etcdutil"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type tlsData struct {
	certData []byte
	keyData  []byte
	caData   []byte
}

func getTLSDataFromSecret(kubecli kubernetes.Interface, ns, se string) (*tlsData, error) {
	secret, err := kubecli.CoreV1().Secrets(ns).Get(se, v1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return &tlsData{
		certData: secret.Data[etcdutil.CliCertFile],
		keyData:  secret.Data[etcdutil.CliKeyFile],
		caData:   secret.Data[etcdutil.CliCAFile],
	}, nil
}

func newCertPool(caData []byte) (*x509.CertPool, error) {
	certPool := x509.NewCertPool()

	for {
		var block *pem.Block
		block, caData = pem.Decode(caData)
		if block == nil {
			break
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		certPool.AddCert(cert)
	}
	return certPool, nil
}

func newTLSConfig(certData, keyData, caData []byte) (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}
	cfg.GetCertificate = func(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
		tlsCert, err := tls.X509KeyPair(certData, keyData)
		if err != nil {
			return nil, err
		}
		return &tlsCert, nil
	}
	cfg.GetClientCertificate = func(_ *tls.CertificateRequestInfo) (*tls.Certificate, error) {
		tlsCert, err := tls.X509KeyPair(certData, keyData)
		if err != nil {
			return nil, err
		}
		return &tlsCert, nil
	}

	var err error
	cfg.RootCAs, err = newCertPool(caData)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func getEtcdOperatorSecrets(ns, name string) (*tls.Config, error) {
	s, err := getTLSDataFromSecret(k8sclient.Client(), ns, name)
	if err != nil {
		return nil, err
	}
	return newTLSConfig(s.certData, s.keyData, s.caData)
}
