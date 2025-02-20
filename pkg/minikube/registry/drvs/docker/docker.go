/*
Copyright 2019 The Kubernetes Authors All rights reserved.

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

package docker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/blang/semver/v4"
	"github.com/docker/machine/libmachine/drivers"
	"github.com/pkg/errors"
	"github.com/spf13/viper"
	"k8s.io/klog/v2"
	"k8s.io/minikube/pkg/drivers/kic"
	"k8s.io/minikube/pkg/drivers/kic/oci"
	"k8s.io/minikube/pkg/minikube/config"
	"k8s.io/minikube/pkg/minikube/driver"
	"k8s.io/minikube/pkg/minikube/localpath"
	"k8s.io/minikube/pkg/minikube/registry"
)

const (
	docURL                   = "https://minikube.sigs.k8s.io/docs/drivers/docker/"
	minDockerVersion         = "18.09.0"
	recommendedDockerVersion = "20.10.0"
)

func init() {
	if err := registry.Register(registry.DriverDef{
		Name:     driver.Docker,
		Config:   configure,
		Init:     func() drivers.Driver { return kic.NewDriver(kic.Config{OCIBinary: oci.Docker}) },
		Status:   status,
		Default:  true,
		Priority: registry.HighlyPreferred,
	}); err != nil {
		panic(fmt.Sprintf("register failed: %v", err))
	}
}

func configure(cc config.ClusterConfig, n config.Node) (interface{}, error) {
	mounts := make([]oci.Mount, len(cc.ContainerVolumeMounts))
	for i, spec := range cc.ContainerVolumeMounts {
		var err error
		mounts[i], err = oci.ParseMountString(spec)
		if err != nil {
			return nil, err
		}
	}

	extraArgs := []string{}

	for _, port := range cc.ExposedPorts {
		extraArgs = append(extraArgs, "-p", port)
	}

	return kic.NewDriver(kic.Config{
		ClusterName:       cc.Name,
		MachineName:       config.MachineName(cc, n),
		StorePath:         localpath.MiniPath(),
		ImageDigest:       cc.KicBaseImage,
		Mounts:            mounts,
		CPU:               cc.CPUs,
		Memory:            cc.Memory,
		OCIBinary:         oci.Docker,
		APIServerPort:     cc.Nodes[0].Port,
		KubernetesVersion: cc.KubernetesConfig.KubernetesVersion,
		ContainerRuntime:  cc.KubernetesConfig.ContainerRuntime,
		ExtraArgs:         extraArgs,
		Network:           cc.Network,
		Subnet:            cc.Subnet,
		StaticIP:          cc.StaticIP,
		ListenAddress:     cc.ListenAddress,
	}), nil
}

func status() (retState registry.State) {
	_, err := exec.LookPath(oci.Docker)
	if err != nil {
		return registry.State{Error: err, Installed: false, Healthy: false, Fix: "Install Docker", Doc: docURL}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, oci.Docker, "version", "--format", "{{.Server.Os}}-{{.Server.Version}}")
	o, err := cmd.Output()
	if err != nil {
		reason := ""
		if ctx.Err() == context.DeadlineExceeded {
			err = errors.Wrapf(err, "deadline exceeded running %q", strings.Join(cmd.Args, " "))
			reason = "PROVIDER_DOCKER_DEADLINE_EXCEEDED"
		}

		klog.Warningf("docker version returned error: %v", err)

		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr := strings.TrimSpace(string(exitErr.Stderr))
			newErr := fmt.Errorf(`%q %v: %s`, strings.Join(cmd.Args, " "), exitErr, stderr)
			return suggestFix("version", exitErr.ExitCode(), stderr, newErr)
		}

		return registry.State{Reason: reason, Error: err, Installed: true, Healthy: false, Fix: "Restart the Docker service", Doc: docURL}
	}

	var improvement string
	recordImprovement := func(s registry.State) {
		if s.NeedsImprovement && s.Fix != "" {
			improvement = s.Fix
		}
	}
	defer func() {
		if retState.Error == nil && retState.Fix == "" && improvement != "" {
			retState.NeedsImprovement = true
			retState.Fix = improvement
		}
	}()

	klog.Infof("docker version: %s", o)
	if !viper.GetBool("force") {
		s := checkDockerVersion(strings.TrimSpace(string(o))) // remove '\n' from o at the end
		if s.Error != nil {
			return s
		}
		recordImprovement(s)
	}

	si, err := oci.CachedDaemonInfo("docker")
	if err != nil {
		// No known fix because we haven't yet seen a failure here
		return registry.State{Reason: "PROVIDER_DOCKER_INFO_FAILED", Error: errors.Wrap(err, "docker info"), Installed: true, Healthy: false, Doc: docURL}
	}

	for _, serr := range si.Errors {
		return suggestFix("info", -1, serr, fmt.Errorf("docker info error: %s", serr))
	}

	// TODO: validate cgroup v2 delegation when si.Rootless is true

	return checkNeedsImprovement()
}

func checkDockerVersion(o string) registry.State {
	parts := strings.SplitN(o, "-", 2)
	if len(parts) != 2 {
		return registry.State{
			Reason:    "PROVIDER_DOCKER_VERSION_PARSING_FAILED",
			Error:     errors.Errorf("expected version string format is \"{{.Server.Os}}-{{.Server.Version}}\". but got %s", o),
			Installed: true,
			Healthy:   false,
			Doc:       docURL,
		}
	}

	if parts[0] == "windows" {
		return registry.State{
			Reason:    "PROVIDER_DOCKER_WINDOWS_CONTAINERS",
			Error:     oci.ErrWindowsContainers,
			Installed: true,
			Healthy:   false,
			Fix:       "Change container type to \"linux\" in Docker Desktop settings",
			Doc:       docURL + "#verify-docker-container-type-is-linux",
		}
	}

	versionMsg := fmt.Sprintf("(Minimum recommended version is %s, minimum supported version is %s, current version is %s)", recommendedDockerVersion, minDockerVersion, parts[1])
	hintInstallOfficial := fmt.Sprintf("Install the official release of %s %s", driver.FullName(driver.Docker), versionMsg)
	hintUpdate := fmt.Sprintf("Upgrade %s to a newer version %s", driver.FullName(driver.Docker), versionMsg)

	p := strings.SplitN(parts[1], ".", 3)
	switch l := len(p); l {
	case 2:
		p = append(p, "0") // patch version not found
	case 3:
		// remove postfix string for unstable(test/nightly) channel. https://docs.docker.com/engine/install/
		p[2] = strings.SplitN(p[2], "-", 2)[0]
	default:
		// When Docker (Moby) was installed from the source code, the version string is typically set to "dev", or "library-import".
		return registry.State{
			Installed:        true,
			Healthy:          true,
			NeedsImprovement: true,
			Fix:              hintInstallOfficial,
			Doc:              docURL,
		}
	}

	currSemver, err := semver.ParseTolerant(strings.Join(p, "."))
	if err != nil {
		return registry.State{
			Installed:        true,
			Healthy:          true,
			NeedsImprovement: true,
			Fix:              hintInstallOfficial,
			Doc:              docURL,
		}
	}
	// these values are consts and their conversions are covered in unit tests
	minSemver, _ := semver.ParseTolerant(minDockerVersion)
	recSemver, _ := semver.ParseTolerant(recommendedDockerVersion)

	if currSemver.GTE(recSemver) {
		return registry.State{Installed: true, Healthy: true, Error: nil}
	}
	if currSemver.GTE(minSemver) {
		return registry.State{
			Installed:        true,
			Healthy:          true,
			NeedsImprovement: true,
			Fix:              hintUpdate,
			Doc:              docURL + "#requirements"}
	}

	return registry.State{
		Reason:           "PROVIDER_DOCKER_VERSION_LOW",
		Error:            oci.ErrMinDockerVersion,
		Installed:        true,
		Healthy:          false,
		NeedsImprovement: true,
		Fix:              hintUpdate,
		Doc:              docURL + "#requirements"}
}

// checkNeedsImprovement if overlay mod is installed on a system
func checkNeedsImprovement() registry.State {
	if runtime.GOOS == "linux" {
		return checkOverlayMod()
	}

	return registry.State{Installed: true, Healthy: true}
}

// checkOverlayMod checks if
func checkOverlayMod() registry.State {
	if _, err := os.Stat("/sys/module/overlay"); err == nil {
		klog.Info("overlay module found")
		return registry.State{Installed: true, Healthy: true}
	}

	if _, err := os.Stat("/sys/module/overlay2"); err == nil {
		klog.Info("overlay2 module found")

		return registry.State{Installed: true, Healthy: true}
	}

	klog.Warningf("overlay modules were not found")

	return registry.State{NeedsImprovement: true, Installed: true, Healthy: true, Fix: "enable the overlay Linux kernel module using 'modprobe overlay'"}
}

// suggestFix matches a stderr with possible fix for the docker driver
func suggestFix(src string, exitcode int, stderr string, err error) registry.State {
	if strings.Contains(stderr, "permission denied") && runtime.GOOS == "linux" {
		return registry.State{Reason: "PROVIDER_DOCKER_NEWGRP", Error: err, Installed: true, Running: true, Healthy: false, Fix: "Add your user to the 'docker' group: 'sudo usermod -aG docker $USER && newgrp docker'", Doc: "https://docs.docker.com/engine/install/linux-postinstall/"}
	}

	if strings.Contains(stderr, "pipe.*docker_engine.*: The system cannot find the file specified.") && runtime.GOOS == "windows" {
		return registry.State{Reason: "PROVIDER_DOCKER_PIPE_NOT_FOUND", Error: err, Installed: true, Running: false, Healthy: false, Fix: "Start the Docker service. If Docker is already running, you may need to reset Docker to factory settings with: Settings > Reset.", Doc: "https://github.com/docker/for-win/issues/1825#issuecomment-450501157"}
	}

	reason := dockerNotRunning(stderr)
	if reason != "" {
		return registry.State{Reason: reason, Error: err, Installed: true, Running: false, Healthy: false, Fix: "Start the Docker service", Doc: docURL}
	}

	// We don't have good advice, but at least we can provide a good error message
	reason = strings.ToUpper(fmt.Sprintf("PROVIDER_DOCKER_%s_ERROR", src))
	if exitcode > 0 {
		reason = strings.ToUpper(fmt.Sprintf("PROVIDER_DOCKER_%s_EXIT_%d", src, exitcode))
	}
	return registry.State{Reason: reason, Error: err, Installed: true, Running: true, Healthy: false, Doc: docURL}
}

// Return a reason code for Docker not running
func dockerNotRunning(s string) string {
	// These codes are explicitly in order of the most likely to be helpful to a user

	if strings.Contains(s, "Is the docker daemon running") || strings.Contains(s, "docker daemon is not running") {
		return "PROVIDER_DOCKER_NOT_RUNNING"
	}

	if strings.Contains(s, "Cannot connect") {
		return "PROVIDER_DOCKER_CANNOT_CONNECT"
	}

	if strings.Contains(s, "refused") {
		return "PROVIDER_DOCKER_REFUSED"
	}

	return ""
}
