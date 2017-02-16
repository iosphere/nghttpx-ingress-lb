package controller

import (
	"encoding/json"

	"github.com/golang/glog"

	"github.com/zlabjp/nghttpx-ingress-lb/pkg/nghttpx"
)

const (
	// backendConfigKey is a key to annotation for extra backend configuration.
	backendConfigKey = "ingress.zlab.co.jp/backend-config"
)

type ingressAnnotation map[string]string

func (ia ingressAnnotation) getBackendConfig() map[string]map[string]nghttpx.PortBackendConfig {
	data := ia[backendConfigKey]
	// the first key specifies service name, and secondary key specifies port name.
	var config map[string]map[string]nghttpx.PortBackendConfig
	if data == "" {
		return config
	}
	if err := json.Unmarshal([]byte(data), &config); err != nil {
		glog.Errorf("unexpected error reading %v annotation: %v", backendConfigKey, err)
		return config
	}

	return config
}
