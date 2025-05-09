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

// Package logs are convenience methods for fetching logs from a minikube cluster
package logs

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"github.com/pkg/errors"
	"k8s.io/klog/v2"
	"k8s.io/minikube/pkg/minikube/assets"
	"k8s.io/minikube/pkg/minikube/audit"
	"k8s.io/minikube/pkg/minikube/bootstrapper"
	"k8s.io/minikube/pkg/minikube/command"
	"k8s.io/minikube/pkg/minikube/config"
	"k8s.io/minikube/pkg/minikube/cruntime"
	"k8s.io/minikube/pkg/minikube/localpath"
	"k8s.io/minikube/pkg/minikube/out"
	"k8s.io/minikube/pkg/minikube/style"
)

// rootCauses are regular expressions that match known failures
var rootCauses = []string{
	`^error: `,
	`eviction manager: pods.* evicted`,
	`unknown flag: --`,
	`forbidden.*no providers available`,
	`eviction manager:.*evicted`,
	`tls: bad certificate`,
	`kubelet.*no API client`,
	`kubelet.*No api server`,
	`STDIN.*127.0.0.1:8080`,
	`failed to create listener`,
	`address already in use`,
	`unable to evict any pods`,
	`eviction manager: unexpected error`,
	`Resetting AnonymousAuth to false`,
	`Unable to register node.*forbidden`,
	`Failed to initialize CSINodeInfo.*forbidden`,
	`Failed to admit pod`,
	`failed to "StartContainer"`,
	`Failed to start ContainerManager`,
	`kubelet.*forbidden.*cannot \w+ resource`,
	`leases.*forbidden.*cannot \w+ resource`,
	`failed to start daemon`,
}

// rootCauseRe combines rootCauses into a single regex
var rootCauseRe = regexp.MustCompile(strings.Join(rootCauses, "|"))

// ignoreCauseRe is a regular expression that matches spurious errors to not surface
var ignoreCauseRe = regexp.MustCompile("error: no objects passed to apply")

// importantPods are a list of pods to retrieve logs for, in addition to the bootstrapper logs.
var importantPods = []string{
	"kube-apiserver",
	"etcd",
	"coredns",
	"kube-scheduler",
	"kube-proxy",
	"kube-controller-manager",
	"kindnet",
}

// logRunner is the subset of CommandRunner used for logging
type logRunner interface {
	RunCmd(*exec.Cmd) (*command.RunResult, error)
}

// lookbackwardsCount is how far back to look in a log for problems. This should be large enough to
// include usage messages from a failed binary, but small enough to not include irrelevant problems.
const lookBackwardsCount = 400

// Follow follows logs from multiple files in tail(1) format
func Follow(r cruntime.Manager, bs bootstrapper.Bootstrapper, cfg config.ClusterConfig, cr logRunner, logOutput io.Writer) error {
	cs := []string{}
	for _, v := range logCommands(r, bs, cfg, 0, true) {
		cs = append(cs, v+" &")
	}
	cs = append(cs, "wait")

	cmd := exec.Command("/bin/bash", "-c", strings.Join(cs, " "))
	cmd.Stdout = logOutput
	cmd.Stderr = logOutput
	if _, err := cr.RunCmd(cmd); err != nil {
		return errors.Wrapf(err, "log follow")
	}
	return nil
}

// IsProblem returns whether this line matches a known problem
func IsProblem(line string) bool {
	return rootCauseRe.MatchString(line) && !ignoreCauseRe.MatchString(line)
}

// FindProblems finds possible root causes among the logs
func FindProblems(r cruntime.Manager, bs bootstrapper.Bootstrapper, cfg config.ClusterConfig, cr logRunner) map[string][]string {
	pMap := map[string][]string{}
	cmds := logCommands(r, bs, cfg, lookBackwardsCount, false)
	for name := range cmds {
		klog.Infof("Gathering logs for %s ...", name)
		var b bytes.Buffer
		c := exec.Command("/bin/bash", "-c", cmds[name])
		c.Stderr = &b
		c.Stdout = &b

		if rr, err := cr.RunCmd(c); err != nil {
			klog.Warningf("failed %s: command: %s %v output: %s", name, rr.Command(), err, rr.Output())
			continue
		}
		scanner := bufio.NewScanner(&b)
		problems := []string{}
		for scanner.Scan() {
			l := scanner.Text()
			if IsProblem(l) {
				klog.Warningf("Found %s problem: %s", name, l)
				problems = append(problems, l)
			}
		}
		if err := scanner.Err(); err != nil {
			klog.Warningf("failed to read output: %v", err)
		}
		if len(problems) > 0 {
			pMap[name] = problems
		}
	}
	return pMap
}

// OutputProblems outputs discovered problems.
func OutputProblems(problems map[string][]string, maxLines int, logOutput *os.File) {
	out.SetErrFile(logOutput)
	defer out.SetErrFile(os.Stderr)

	for name, lines := range problems {
		out.FailureT("Problems detected in {{.name}}:", out.V{"name": name})
		if len(lines) > maxLines {
			lines = lines[len(lines)-maxLines:]
		}
		for _, l := range lines {
			out.ErrT(style.LogEntry, l)
		}
	}
}

// Output displays logs from multiple sources in tail(1) format
func Output(r cruntime.Manager, bs bootstrapper.Bootstrapper, cfg config.ClusterConfig, runner command.Runner, lines int, logOutput *os.File) {
	cmds := logCommands(r, bs, cfg, lines, false)
	cmds["kernel"] = "uptime && uname -a && grep PRETTY /etc/os-release"

	names := []string{}
	for k := range cmds {
		names = append(names, k)
	}

	out.SetOutFile(logOutput)
	defer out.SetOutFile(os.Stdout)
	out.SetErrFile(logOutput)
	defer out.SetErrFile(os.Stderr)

	sort.Strings(names)
	for i, name := range names {
		if i > 0 {
			out.Styled(style.None, "")
		}
		out.Styled(style.None, "==> {{.name}} <==", out.V{"name": name})
		var b bytes.Buffer
		c := exec.Command("/bin/bash", "-c", cmds[name])
		c.Stdout = &b
		c.Stderr = &b
		if rr, err := runner.RunCmd(c); err != nil {
			out.Styled(style.None, fmt.Sprintf("command %s failed with error: %v", rr.Command(), err))
			continue
		}
		l := ""
		scanner := bufio.NewScanner(&b)
		for scanner.Scan() {
			l += scanner.Text() + "\n"
		}
		if err := scanner.Err(); err != nil {
			l += fmt.Sprintf("failed to read output: %v", err)
		}
		out.Styled(style.None, l)
	}
}

// OutputAudit displays the audit logs.
func OutputAudit(lines int) error {
	out.Styled(style.None, "")
	out.Styled(style.None, "==> Audit <==")
	r, err := audit.Report(lines)
	if err != nil {
		return fmt.Errorf("failed to create audit report: %v", err)
	}
	out.Styled(style.None, r.ASCIITable())
	return nil
}

// OutputLastStart outputs the last start logs.
func OutputLastStart() error {
	out.Styled(style.None, "")
	out.Styled(style.None, "==> Last Start <==")
	fp := localpath.LastStartLog()
	f, err := os.Open(fp)
	if os.IsNotExist(err) {
		msg := fmt.Sprintf("Last start log file not found at %s", fp)
		out.Styled(style.None, msg)
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to open file %s: %v", fp, err)
	}
	defer f.Close()
	l := ""
	r := bufio.NewReader(f)
	var s string
	for {
		s, err = r.ReadString('\n')
		if err != nil {
			break
		}
		l += s
	}
	out.Styled(style.None, l)
	if err == io.EOF {
		return nil
	}

	return fmt.Errorf("failed to read file %s: %v", fp, err)
}

// OutputOffline outputs logs that don't need a running cluster.
func OutputOffline(lines int, logOutput *os.File) {
	out.SetOutFile(logOutput)
	defer out.SetOutFile(os.Stdout)
	out.SetErrFile(logOutput)
	defer out.SetErrFile(os.Stderr)
	if err := OutputAudit(lines); err != nil {
		klog.Errorf("failed to output audit logs: %v", err)
	}
	if err := OutputLastStart(); err != nil {
		klog.Errorf("failed to output last start logs: %v", err)
	}

	out.Styled(style.None, "")
}

// logCommands returns a list of commands that would be run to receive the anticipated logs
func logCommands(r cruntime.Manager, bs bootstrapper.Bootstrapper, cfg config.ClusterConfig, length int, follow bool) map[string]string {
	cmds := bs.LogCommands(cfg, bootstrapper.LogOptions{Lines: length, Follow: follow})
	pods := importantPods
	addonPods := enabledAddonPods(cfg)
	pods = append(pods, addonPods...)
	for _, pod := range pods {
		ids, err := r.ListContainers(cruntime.ListContainersOptions{Name: pod})
		if err != nil {
			klog.Errorf("Failed to list containers for %q: %v", pod, err)
			continue
		}
		klog.Infof("%d containers: %s", len(ids), ids)
		if len(ids) == 0 {
			klog.Warningf("No container was found matching %q", pod)
			continue
		}
		for _, i := range ids {
			key := fmt.Sprintf("%s [%s]", pod, i)
			cmds[key] = r.ContainerLogCmd(i, length, follow)
		}
	}
	cmds[r.Name()] = r.SystemLogCmd(length)
	cmds["container status"] = cruntime.ContainerStatusCommand()

	return cmds
}

// enabledAddonPods returns the pod names for enabled addons
// this does not currently include all addons, mostly just addons that we occasionally get users reporting issues with
func enabledAddonPods(cfg config.ClusterConfig) []string {
	addonPodMap := map[string]string{
		"dashboard":           "kubernetes-dashboard",
		"gcp-auth":            "gcp-auth",
		"ingress":             "controller_ingress",
		"storage-provisioner": "storage-provisioner",
	}
	addonsPods := []string{}
	for addon, podName := range addonPodMap {
		if assets.Addons[addon].IsEnabled(&cfg) {
			addonsPods = append(addonsPods, podName)
		}
	}
	return addonsPods
}
