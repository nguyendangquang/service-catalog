/*
Copyright 2018 The Kubernetes Authors.

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

package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"text/template"

	"github.com/kubernetes-incubator/service-catalog/cmd/svcat/plugin"
	"github.com/kubernetes-incubator/service-catalog/internal/test"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"
)

var catalogRequestRegex = regexp.MustCompile("/apis/servicecatalog.k8s.io/v1beta1/(.*)")

func TestCommandOutput(t *testing.T) {
	testcases := []struct {
		name            string // Test Name
		cmd             string // Command to run
		golden          string // Relative path to a golden file, compared to the command output
		continueOnError bool   // Should the test stop immediately if the command fails or continue and capture the console output
	}{
		{name: "list all brokers", cmd: "get brokers", golden: "output/get-brokers.txt"},
		{name: "get broker", cmd: "get broker ups-broker", golden: "output/get-broker.txt"},
		{name: "describe broker", cmd: "describe broker ups-broker", golden: "output/describe-broker.txt"},

		{name: "list all classes", cmd: "get classes", golden: "output/get-classes.txt"},
		{name: "get class by name", cmd: "get class user-provided-service", golden: "output/get-class.txt"},
		{name: "get class by uuid", cmd: "get class --uuid 4f6e6cf6-ffdd-425f-a2c7-3c9258ad2468", golden: "output/get-class.txt"},
		{name: "describe class by name", cmd: "describe class user-provided-service", golden: "output/describe-class.txt"},
		{name: "describe class uuid", cmd: "describe class --uuid 4f6e6cf6-ffdd-425f-a2c7-3c9258ad2468", golden: "output/describe-class.txt"},

		{name: "list all plans", cmd: "get plans", golden: "output/get-plans.txt"},
		{name: "get plan by name", cmd: "get plan default", golden: "output/get-plan.txt"},
		{name: "get plan by uuid", cmd: "get plan --uuid 86064792-7ea2-467b-af93-ac9694d96d52", golden: "output/get-plan.txt"},
		{name: "describe plan by name", cmd: "describe plan default", golden: "output/describe-plan.txt"},
		{name: "describe plan by uuid", cmd: "describe plan --uuid 86064792-7ea2-467b-af93-ac9694d96d52", golden: "output/describe-plan.txt"},

		{name: "list all instances", cmd: "get instances -n test-ns", golden: "output/get-instances.txt"},
		{name: "get instance", cmd: "get instance ups-instance -n test-ns", golden: "output/get-instance.txt"},
		{name: "describe instance", cmd: "describe instance ups-instance -n test-ns", golden: "output/describe-instance.txt"},

		{name: "list all bindings", cmd: "get bindings -n test-ns", golden: "output/get-bindings.txt"},
		{name: "get binding", cmd: "get binding ups-binding -n test-ns", golden: "output/get-binding.txt"},
		{name: "describe binding", cmd: "describe binding ups-binding -n test-ns", golden: "output/describe-binding.txt"},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			output := executeCommand(t, tc.cmd, tc.continueOnError)
			test.AssertEqualsGoldenFile(t, tc.golden, output)
		})
	}
}

func TestGenerateManifest(t *testing.T) {
	svcat := buildRootCommand()

	m := &plugin.Manifest{}
	m.Load(svcat)

	got, err := yaml.Marshal(&m)
	if err != nil {
		t.Fatalf("%+v", err)
	}

	test.AssertEqualsGoldenFile(t, "plugin.yaml", string(got))
}

// executeCommand runs a svcat command against a fake k8s api,
// returning the cli output.
func executeCommand(t *testing.T, cmd string, continueOnErr bool) string {
	// Fake the k8s api server
	apisvr := newAPIServer()
	defer apisvr.Close()

	// Generate a test kubeconfig pointing at the server
	kubeconfig, err := writeTestKubeconfig(apisvr.URL)
	if err != nil {
		t.Fatalf("%+v", err)
	}
	defer os.Remove(kubeconfig)

	// Setup the svcat command
	svcat := buildRootCommand()
	args := strings.Split(cmd, " ")
	args = append(args, "--kubeconfig", kubeconfig)
	svcat.SetArgs(args)

	// Capture all output: stderr and stdout
	output := &bytes.Buffer{}
	svcat.SetOutput(output)

	err = svcat.Execute()
	if err != nil && !continueOnErr {
		t.Fatalf("%+v", err)
	}

	return output.String()
}

func newAPIServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(apihandler))
}

// apihandler handles requests to the service catalog endpoint.
// When a request is received, it looks up the response from the testdata directory.
// Example:
// GET /apis/servicecatalog.k8s.io/v1beta1/clusterservicebrokers responds with testdata/clusterservicebrokers.json
func apihandler(w http.ResponseWriter, r *http.Request) {
	match := catalogRequestRegex.FindStringSubmatch(r.RequestURI)

	if len(match) == 0 {
		w.WriteHeader(500)
		w.Write([]byte(fmt.Sprintf("unexpected request %s %s", r.Method, r.RequestURI)))
	}

	if r.Method != http.MethodGet {
		// Anything more interesting than a GET, i.e. it relies upon server behavior
		// probably should be an integration test instead
		w.WriteHeader(500)
		w.Write([]byte(fmt.Sprintf("unexpected request %s %s", r.Method, r.RequestURI)))
	}

	relpath, err := url.PathUnescape(match[1])
	if err != nil {
		w.WriteHeader(500)
		w.Write([]byte(fmt.Sprintf("could not unescape path %s (%s)", match[1], err)))
	}
	_, response, err := test.GetTestdata(filepath.Join("responses", relpath+".json"))
	if err != nil {
		w.WriteHeader(500)
		w.Write([]byte(fmt.Sprintf("unexpected request %s with no matching testdata (%s)", r.RequestURI, err)))
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(response)
}

func writeTestKubeconfig(fakeURL string) (string, error) {
	_, configT, err := test.GetTestdata("kubeconfig.tmpl.yaml")
	if err != nil {
		return "", err
	}

	data := map[string]string{
		"Server": fakeURL,
	}
	t := template.Must(template.New("kubeconfig").Parse(string(configT)))

	f, err := ioutil.TempFile("", "kubeconfig")
	if err != nil {
		return "", errors.Wrap(err, "unable to create a temporary kubeconfig file")
	}
	defer f.Close()

	err = t.Execute(f, data)
	return f.Name(), errors.Wrap(err, "error executing the kubeconfig template")
}
