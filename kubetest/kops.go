/*
Copyright 2017 The Kubernetes Authors.

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
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var (
	// kops specific flags.
	kopsPath         = flag.String("kops", "", "(kops only) Path to the kops binary. kops will be downloaded from kops-base-url if not set.")
	kopsCluster      = flag.String("kops-cluster", "", "(kops only) Deprecated. Cluster name for kops; if not set defaults to --cluster.")
	kopsState        = flag.String("kops-state", "", "(kops only) s3:// path to kops state store. Must be set.")
	kopsSSHKey       = flag.String("kops-ssh-key", "", "(kops only) Path to ssh key-pair for each node (defaults '~/.ssh/kube_aws_rsa' if unset.)")
	kopsKubeVersion  = flag.String("kops-kubernetes-version", "", "(kops only) If set, the version of Kubernetes to deploy (can be a URL to a GCS path where the release is stored) (Defaults to kops default, latest stable release.).")
	kopsZones        = flag.String("kops-zones", "us-west-2a", "(kops only) zones for kops deployment, comma delimited.")
	kopsNodes        = flag.Int("kops-nodes", 2, "(kops only) Number of nodes to create.")
	kopsUpTimeout    = flag.Duration("kops-up-timeout", 20*time.Minute, "(kops only) Time limit between 'kops config / kops update' and a response from the Kubernetes API.")
	kopsAdminAccess  = flag.String("kops-admin-access", "", "(kops only) If set, restrict apiserver access to this CIDR range.")
	kopsImage        = flag.String("kops-image", "", "(kops only) Image (AMI) for nodes to use. (Defaults to kops default, a Debian image with a custom kubernetes kernel.)")
	kopsArgs         = flag.String("kops-args", "", "(kops only) Additional space-separated args to pass unvalidated to 'kops create cluster', e.g. '--kops-args=\"--dns private --node-size t2.micro\"'")
	kopsPriorityPath = flag.String("kops-priority-path", "", "Insert into PATH if set")
	kopsBaseURL      = flag.String("kops-base-url", "", "Base URL for a prebuilt version of kops")
	kopsVersion      = flag.String("kops-version", "", "URL to a file containing a valid kops-base-url")
)

type kops struct {
	path        string
	kubeVersion string
	sshKey      string
	zones       []string
	nodes       int
	adminAccess string
	cluster     string
	image       string
	args        string
	kubecfg     string

	// GCP project we should use
	gcpProject string

	// Cloud provider in use (gce, aws)
	provider string
}

var _ deployer = kops{}

func migrateKopsEnv() error {
	return migrateOptions([]migratedOption{
		{
			env:      "KOPS_STATE_STORE",
			option:   kopsState,
			name:     "--kops-state",
			skipPush: true,
		},
		{
			env:      "AWS_SSH_KEY",
			option:   kopsSSHKey,
			name:     "--kops-ssh-key",
			skipPush: true,
		},
		{
			env:      "PRIORITY_PATH",
			option:   kopsPriorityPath,
			name:     "--kops-priority-path",
			skipPush: true,
		},
	})
}

func newKops(provider, gcpProject, cluster string) (*kops, error) {
	tmpdir, err := ioutil.TempDir("", "kops")
	if err != nil {
		return nil, err
	}

	if err := migrateKopsEnv(); err != nil {
		return nil, err
	}

	if *kopsCluster != "" {
		cluster = *kopsCluster
	}
	if cluster == "" {
		return nil, fmt.Errorf("--cluster or --kops-cluster must be set to a valid cluster name for kops deployment")
	}
	if *kopsState == "" {
		return nil, fmt.Errorf("--kops-state must be set to a valid S3 path for kops deployment")
	}
	if *kopsPriorityPath != "" {
		if err := insertPath(*kopsPriorityPath); err != nil {
			return nil, err
		}
	}

	// TODO(fejta): consider explicitly passing these env items where needed.
	sshKey := *kopsSSHKey
	if sshKey == "" {
		usr, err := user.Current()
		if err != nil {
			return nil, err
		}
		sshKey = filepath.Join(usr.HomeDir, ".ssh/kube_aws_rsa")
	}
	if err := os.Setenv("KOPS_STATE_STORE", *kopsState); err != nil {
		return nil, err
	}

	// Repoint KUBECONFIG to an isolated kubeconfig in our temp directory
	kubecfg := filepath.Join(tmpdir, "kubeconfig")
	f, err := os.Create(kubecfg)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := f.Chmod(0600); err != nil {
		return nil, err
	}
	if err := os.Setenv("KUBECONFIG", kubecfg); err != nil {
		return nil, err
	}

	// Set KUBERNETES_CONFORMANCE_TEST so the auth info is picked up
	// from kubectl instead of bash inference.
	if err := os.Setenv("KUBERNETES_CONFORMANCE_TEST", "yes"); err != nil {
		return nil, err
	}
	// Set KUBERNETES_CONFORMANCE_PROVIDER to override the
	// cloudprovider for KUBERNETES_CONFORMANCE_TEST.
	if err := os.Setenv("KUBERNETES_CONFORMANCE_PROVIDER", "aws"); err != nil {
		return nil, err
	}
	// AWS_SSH_KEY is required by the AWS e2e tests.
	if err := os.Setenv("AWS_SSH_KEY", sshKey); err != nil {
		return nil, err
	}
	// ZONE is required by the AWS e2e tests.
	zones := strings.Split(*kopsZones, ",")
	if err := os.Setenv("ZONE", zones[0]); err != nil {
		return nil, err
	}

	// Set kops-base-url from kops-version
	if *kopsVersion != "" {
		if *kopsBaseURL != "" {
			return nil, fmt.Errorf("cannot set --kops-version and --kops-base-url")
		}

		var b bytes.Buffer
		if err := httpRead(*kopsVersion, &b); err != nil {
			return nil, err
		}
		latest := strings.TrimSpace(b.String())

		log.Printf("Got latest kops version from %v: %v", *kopsVersion, latest)
		if latest == "" {
			return nil, fmt.Errorf("version URL %v was empty", *kopsVersion)
		}
		*kopsBaseURL = latest
	}

	// kops looks at KOPS_BASE_URL env var, so export it here
	if *kopsBaseURL != "" {
		if err := os.Setenv("KOPS_BASE_URL", *kopsBaseURL); err != nil {
			return nil, err
		}
	}

	// Download kops from kopsBaseURL if kopsPath is not set
	if *kopsPath == "" {
		if *kopsBaseURL == "" {
			return nil, fmt.Errorf("--kops or --kops-base-url must be set")
		}

		kopsBinURL := *kopsBaseURL + "/linux/amd64/kops"
		log.Printf("Download kops binary from %s", kopsBinURL)
		kopsBin := filepath.Join(tmpdir, "kops")
		f, err := os.Create(kopsBin)
		if err != nil {
			return nil, fmt.Errorf("error creating file %q: %v", kopsBin, err)
		}
		defer f.Close()
		if err := httpRead(kopsBinURL, f); err != nil {
			return nil, err
		}
		if err := ensureExecutable(kopsBin); err != nil {
			return nil, err
		}
		*kopsPath = kopsBin
	}

	return &kops{
		path:        *kopsPath,
		kubeVersion: *kopsKubeVersion,
		sshKey:      sshKey + ".pub", // kops only needs the public key, e2es need the private key.
		zones:       zones,
		nodes:       *kopsNodes,
		adminAccess: *kopsAdminAccess,
		cluster:     cluster,
		image:       *kopsImage,
		args:        *kopsArgs,
		kubecfg:     kubecfg,
		provider:    provider,
		gcpProject:  gcpProject,
	}, nil
}

func (k kops) isGoogleCloud() bool {
	return k.provider == "gce"
}

func (k kops) Up() error {
	// If we downloaded kubernetes, pass that version to kops
	if k.kubeVersion == "" {
		// TODO(justinsb): figure out a refactor that allows us to get this from acquireKubernetes cleanly
		kubeReleaseUrl := os.Getenv("KUBERNETES_RELEASE_URL")
		kubeRelease := os.Getenv("KUBERNETES_RELEASE")
		if kubeReleaseUrl != "" && kubeRelease != "" {
			if !strings.HasSuffix(kubeReleaseUrl, "/") {
				kubeReleaseUrl += "/"
			}
			k.kubeVersion = kubeReleaseUrl + kubeRelease
		}
	}

	var featureFlags []string

	createArgs := []string{
		"create", "cluster",
		"--name", k.cluster,
		"--ssh-public-key", k.sshKey,
		"--node-count", strconv.Itoa(k.nodes),
		"--zones", strings.Join(k.zones, ","),
	}
	if k.kubeVersion != "" {
		createArgs = append(createArgs, "--kubernetes-version", k.kubeVersion)
	}
	if k.adminAccess != "" {
		createArgs = append(createArgs, "--admin-access", k.adminAccess)

		// Enable nodeport access from the same IP (we expect it to be the test IPs)
		featureFlags = append(featureFlags, "SpecOverrideFlag")
		createArgs = append(createArgs, "--override", "cluster.spec.nodePortAccess="+k.adminAccess)
	}
	if k.image != "" {
		createArgs = append(createArgs, "--image", k.image)
	}
	if k.gcpProject != "" {
		createArgs = append(createArgs, "--project", k.gcpProject)
	}
	if k.isGoogleCloud() {
		featureFlags = append(featureFlags, "AlphaAllowGCE")
	}
	if k.args != "" {
		createArgs = append(createArgs, strings.Split(k.args, " ")...)
	}

	if len(featureFlags) != 0 {
		os.Setenv("KOPS_FEATURE_FLAGS", strings.Join(featureFlags, ","))
	}

	if err := finishRunning(exec.Command(k.path, createArgs...)); err != nil {
		return fmt.Errorf("kops configuration failed: %v", err)
	}
	if err := finishRunning(exec.Command(k.path, "update", "cluster", k.cluster, "--yes")); err != nil {
		return fmt.Errorf("kops bringup failed: %v", err)
	}
	// TODO(zmerlynn): More cluster validation. This should perhaps be
	// added to kops and not here, but this is a fine place to loop
	// for now.
	return waitForReadyNodes(k.nodes+1, *kopsUpTimeout)
}

func (k kops) IsUp() error {
	return isUp(k)
}

func (k kops) DumpClusterLogs(localPath, gcsPath string) error {
	return defaultDumpClusterLogs(localPath, gcsPath)
}

func (k kops) TestSetup() error {
	info, err := os.Stat(k.kubecfg)
	if err != nil {
		return err
	}
	if info.Size() > 0 {
		// Assume that if we already have it, it's good.
		return nil
	}
	if err := finishRunning(exec.Command(k.path, "export", "kubecfg", k.cluster)); err != nil {
		return fmt.Errorf("Failure exporting kops kubecfg: %v", err)
	}
	return nil
}

func (k kops) Down() error {
	// We do a "kops get" first so the exit status of "kops delete" is
	// more sensical in the case of a non-existent cluster. ("kops
	// delete" will exit with status 1 on a non-existent cluster)
	err := finishRunning(exec.Command(k.path, "get", "clusters", k.cluster))
	if err != nil {
		// This is expected if the cluster doesn't exist.
		return nil
	}
	return finishRunning(exec.Command(k.path, "delete", "cluster", k.cluster, "--yes"))
}

func (k kops) GetClusterCreated(gcpProject string) (time.Time, error) {
	return time.Time{}, errors.New("not implemented")
}
