/*
Copyright 2015 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

/**
 * Copyright 2016, Z Lab Corporation. All rights reserved.
 *
 * For the full copyright and license information, please view the LICENSE
 * file that was distributed with this source code.
 */

package nghttpx

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"time"

	"k8s.io/kubernetes/pkg/util/wait"

	"github.com/golang/glog"
)

const (
	backendconfigURI  = "http://127.0.0.1:3001/api/v1beta1/backendconfig"
	configrevisionURI = "http://127.0.0.1:3001/api/v1beta1/configrevision"
)

// Start starts a nghttpx process, and wait.
func (ngx *Manager) Start(stopCh <-chan struct{}) {
	glog.Info("Starting nghttpx process...")
	cmd := exec.Command("/usr/local/bin/nghttpx")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		glog.Errorf("nghttpx didn't started successfully: %v", err)
		return
	}

	waitDoneCh := make(chan struct{})
	go func() {
		if err := cmd.Wait(); err != nil {
			glog.Errorf("nghttpx didn't complete successfully: %v", err)
		}
		close(waitDoneCh)
	}()

	select {
	case <-waitDoneCh:
		glog.Infof("nghttpx exited")
	case <-stopCh:
		glog.Infof("Sending QUIT signal to nghttpx process (PID %v) to shut down gracefully", cmd.Process.Pid)
		if err := cmd.Process.Signal(syscall.SIGQUIT); err != nil {
			glog.Errorf("Could not send signal to nghttpx process (PID %v): %v", cmd.Process.Pid, err)
		}
		<-waitDoneCh
		glog.Infof("nghttpx exited")
	}
}

// CheckAndReload verify if the nghttpx configuration changed and sends a reload
//
// The current running nghttpx master process executes new nghttpx
// with new configuration.  If its invocation succeeds, current
// nghttpx is going to shutdown gracefully.  The invocation of new
// process may fail due to invalid configurations.
func (ngx *Manager) CheckAndReload(ingressCfg *IngressConfig) (bool, error) {
	mainConfig, backendConfig, err := ngx.generateCfg(ingressCfg)
	if err != nil {
		return false, err
	}

	changed, err := ngx.checkAndWriteCfg(mainConfig, backendConfig)
	if err != nil {
		return false, fmt.Errorf("failed to write new nghttpx configuration. Avoiding reload: %v", err)
	}

	if changed == configNotChanged {
		return false, nil
	}

	if glog.V(3) {
		conf := make(map[string]interface{})
		conf["upstreams"] = ingressCfg.Upstreams
		conf["cfg"] = ingressCfg

		b, err := json.MarshalIndent(conf, "", "  ")
		if err != nil {
			fmt.Println("error:", err)
		}
		glog.Infof("nghttpx configuration: %v", string(b))
	}

	switch changed {
	case mainConfigChanged:
		oldConfRev, err := ngx.getNghttpxConfigRevision()
		if err != nil {
			return false, err
		}
		if err := ngx.writeTLSKeyCert(ingressCfg); err != nil {
			return false, err
		}

		cmd := "killall"
		args := []string{"-HUP", "nghttpx"}
		glog.Info("change in configuration detected. Reloading...")
		out, err := exec.Command(cmd, args...).CombinedOutput()
		if err != nil {
			return false, fmt.Errorf("failed to execute %v %v: %v", cmd, args, string(out))
		}

		if err := ngx.waitUntilConfigRevisionChanges(oldConfRev); err != nil {
			return false, err
		}

		glog.Info("nghttpx has finished reloading new configuration")
	case backendConfigChanged:
		if err := ngx.issueBackendReplaceRequest(); err != nil {
			return false, fmt.Errorf("failed to issue backend replace request: %v", err)
		}
	}

	return true, nil
}

func (ngx *Manager) issueBackendReplaceRequest() error {
	glog.Infof("Issuing API request %v", backendconfigURI)

	in, err := os.Open(ngx.BackendConfigFile)
	if err != nil {
		return fmt.Errorf("Could not open backend configuration file %v: %v", ngx.BackendConfigFile, err)
	}

	defer in.Close()

	req, err := http.NewRequest(http.MethodPost, backendconfigURI, in)
	if err != nil {
		return fmt.Errorf("Could not create API request: %v", err)
	}

	req.Header.Add("Content-Type", "text/plain")

	resp, err := ngx.httpClient.Do(req)

	if err != nil {
		return fmt.Errorf("Could not issue API request: %v", err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("backendconfig API endpoint returned unsuccessful status code %v", resp.StatusCode)
	}

	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("Error while reading API response body: %v", err)
	}

	if glog.V(3) {
		glog.Infof("API request returned response body: %v", string(respBody))
	}

	glog.Info("API request has completed successfully")

	return nil
}

// apiResult is an object to store the result of nghttpx API.
type apiResult struct {
	Status string                 `json:"status,omitempty"`
	Code   int32                  `json:"code,omitempty"`
	Data   map[string]interface{} `json:"data,omitempty"`
}

// getNghttpxConfigRevision returns the current nghttpx configRevision through configrevision API call.
func (ngx *Manager) getNghttpxConfigRevision() (string, error) {
	glog.V(4).Infof("Issuing API request %v", configrevisionURI)

	resp, err := ngx.httpClient.Get(configrevisionURI)
	if err != nil {
		return "", fmt.Errorf("Could not get nghttpx configRevision: %v", err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("configrevision API endpoint returned unsuccessful status code %v", resp.StatusCode)
	}

	d := json.NewDecoder(resp.Body)
	d.UseNumber()

	var r apiResult
	if err := d.Decode(&r); err != nil {
		return "", fmt.Errorf("Could not parse nghttpx configuration API result: %v", err)
	}

	if r.Data == nil {
		return "", fmt.Errorf("nghttpx configuration API result has nil Data field")
	}

	s := r.Data["configRevision"]
	confRev, ok := s.(json.Number)
	if !ok {
		return "", fmt.Errorf("nghttpx configuration API result has non json.Number configRevision")
	}

	glog.V(4).Infof("nghttpx configRevision is %v", confRev)

	return confRev.String(), nil
}

// waitUntilConfigRevisionChanges waits for the current nghttpx configuration to change from old value, oldConfRev.
func (ngx *Manager) waitUntilConfigRevisionChanges(oldConfRev string) error {
	glog.Infof("Waiting for nghttpx to finish reloading configuration")

	if err := wait.Poll(1*time.Second, 30*time.Second, func() (bool, error) {
		if newConfRev, err := ngx.getNghttpxConfigRevision(); err != nil {
			return false, err
		} else if newConfRev == oldConfRev {
			return false, nil
		} else {
			return true, nil
		}
	}); err != nil {
		return fmt.Errorf("Could not get new nghttpx configRevision: %v", err)
	}

	return nil
}
