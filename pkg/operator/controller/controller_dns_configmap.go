package controller

import (
	"bytes"
	"context"
	"fmt"
	"text/template"

	"github.com/openshift/cluster-dns-operator/pkg/manifests"

	"github.com/sirupsen/logrus"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	operatorv1 "github.com/openshift/api/operator/v1"

	corev1 "k8s.io/api/core/v1"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var corefileTemplate = template.Must(template.New("Corefile").Parse(`{{range .Servers -}}
# {{.Name}}
{{range .Zones}}{{.}}:5353 {{end}}{
    {{with .ForwardPlugin -}}
    forward .{{range .Upstreams}} {{.}}{{end}}
    {{- end}}
}
{{end -}}
.:5353 {
    errors
    health
    kubernetes {{.ClusterDomain}} in-addr.arpa ip6.arpa {
        pods insecure
        upstream
        fallthrough in-addr.arpa ip6.arpa
    }
    prometheus :9153
    forward . /etc/resolv.conf {
        policy sequential
    }
    cache {{.GlobalCache}}
    reload
}
`))

// ensureDNSConfigMap ensures that a configmap exists for a given DNS.
func (r *reconciler) ensureDNSConfigMap(dns *operatorv1.DNS, clusterDomain string) (*corev1.ConfigMap, error) {
	current, err := r.currentDNSConfigMap(dns)
	if err != nil {
		return nil, fmt.Errorf("failed to get configmap: %v", err)
	}
	desired, err := desiredDNSConfigMap(dns, clusterDomain)
	if err != nil {
		return nil, fmt.Errorf("failed to build configmap: %v", err)
	}

	switch {
	case desired != nil && current == nil:
		if err := r.client.Create(context.TODO(), desired); err != nil {
			return nil, fmt.Errorf("failed to create configmap: %v", err)
		}
		logrus.Infof("created configmap: %s", desired.Name)
	case desired != nil && current != nil:
		if needsUpdate, updated := corefileChanged(current, desired); needsUpdate {
			if err := r.client.Update(context.TODO(), updated); err != nil {
				return nil, fmt.Errorf("failed to update configmap: %v", err)
			}
			logrus.Infof("updated configmap; old: %#v, new: %#v", current, updated)
		}
	}
	return r.currentDNSConfigMap(dns)
}

func (r *reconciler) currentDNSConfigMap(dns *operatorv1.DNS) (*corev1.ConfigMap, error) {
	current := &corev1.ConfigMap{}
	err := r.client.Get(context.TODO(), DNSConfigMapName(dns), current)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return current, nil
}

func desiredDNSConfigMap(dns *operatorv1.DNS, clusterDomain string) (*corev1.ConfigMap, error) {
	if len(clusterDomain) == 0 {
		clusterDomain = "cluster.local"
	}

	var globalCache uint32
	if dns.Spec.GlobalCache == 0 {
		globalCache = 30
	} else {
		globalCache = dns.Spec.GlobalCache
	}

	corefileParameters := struct {
		ClusterDomain string
		Servers       interface{}
		GlobalCache   uint32
	}{
		ClusterDomain: clusterDomain,
		Servers:       dns.Spec.Servers,
		GlobalCache:   globalCache,
	}
	corefile := new(bytes.Buffer)
	if err := corefileTemplate.Execute(corefile, corefileParameters); err != nil {
		return nil, err
	}

	name := DNSConfigMapName(dns)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name.Name,
			Namespace: name.Namespace,
			Labels: map[string]string{
				manifests.OwningDNSLabel: DNSDaemonSetLabel(dns),
			},
		},
		Data: map[string]string{
			"Corefile": corefile.String(),
		},
	}
	cm.SetOwnerReferences([]metav1.OwnerReference{dnsOwnerRef(dns)})

	return cm, nil
}

func corefileChanged(current, expected *corev1.ConfigMap) (bool, *corev1.ConfigMap) {
	if cmp.Equal(current.Data, expected.Data, cmpopts.EquateEmpty()) {
		return false, current
	}
	updated := current.DeepCopy()
	updated.Data = expected.Data
	return true, updated
}
