//
// Author:: Salim Afiune Maya (<afiune@lacework.net>)
// Copyright:: Copyright 2020, Lacework Inc.
// License:: Apache License, Version 2.0
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
//

package cmd

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/pkg/errors"

	"github.com/lacework/go-sdk/api"
)

var SupportedPackageManagers = []string{"dpkg-query", "rpm"} // @afiune can we support yum and apk?

type OS struct {
	Name    string
	Version string
}

var (
	osReleaseFile = "/etc/os-release"
	rexNameFromID = regexp.MustCompile(`^ID=(.*)$`)
	rexVersionID  = regexp.MustCompile(`^VERSION_ID=(.*)$`)
)

func (c *cliState) GeneratePackageManifest() (*api.PackageManifest, error) {
	var (
		err   error
		start = time.Now()
	)

	defer func() {
		c.Event.DurationMs = time.Since(start).Milliseconds()
		if err == nil {
			// if this function returns an error, most likely,
			// the command will send a honeyvent with that error,
			// therefore we should duplicate events and only send
			// one here if there is NO error
			c.SendHoneyvent()
		}
	}()

	c.Event.Feature = featGenPkgManifest

	manifest := new(api.PackageManifest)
	osInfo, err := c.GetOSInfo()
	if err != nil {
		return manifest, err
	}
	c.Event.AddFeatureField("os", osInfo.Name)
	c.Event.AddFeatureField("os_ver", osInfo.Version)

	manager, err := c.DetectPackageManager()
	if err != nil {
		return manifest, err
	}
	c.Event.AddFeatureField("pkg_manager", manager)

	var managerQuery []byte
	switch manager {
	case "rpm":
		managerQuery, err = exec.Command(
			"rpm", "-qa", "--queryformat", "%{NAME},%|EPOCH?{%{EPOCH}}:{0}|:%{VERSION}-%{RELEASE}\n",
		).Output()
		if err != nil {
			return manifest, errors.Wrap(err, "unable to query packages from package manager")
		}
	case "dpkg-query":
		managerQuery, err = exec.Command(
			"dpkg-query", "--show", "--showformat", "${Package},${Version}\n",
		).Output()
		if err != nil {
			return manifest, errors.Wrap(err, "unable to query packages from package manager")
		}
	case "yum":
		return manifest, errors.New("yum not yet supported")
	case "apk":
		apkInfo, err := exec.Command("apk", "info").Output()
		if err != nil {
			return manifest, errors.Wrap(err, "unable to query packages from package manager")
		}
		apkInfoT := strings.TrimSuffix(string(apkInfo), "\n")
		apkInfoArray := strings.Split(apkInfoT, "\n")

		apkInfoWithVersion, err := exec.Command("apk", "info", "-v").Output()
		if err != nil {
			return manifest, errors.Wrap(err, "unable to query packages from package manager")
		}
		apkInfoWithVersionT := strings.TrimSuffix(string(apkInfoWithVersion), "\n")
		apkInfoWithVersionArray := strings.Split(apkInfoWithVersionT, "\n")

		mq := []string{}
		for i, pkg := range apkInfoWithVersionArray {
			mq = append(mq,
				fmt.Sprintf("%s,%s",
					apkInfoArray[i],
					strings.Trim(
						strings.Replace(pkg, apkInfoArray[i], "", 1),
						"-",
					),
				),
			)
		}
		managerQuery = []byte(strings.Join(mq, "\n"))
	default:
		return manifest, errors.New(
			"this is most likely a mistake on us, please report it to support@lacework.com.",
		)
	}

	c.Log.Debugw("package-manager query", "raw", string(managerQuery))

	// @afiune this is an example of the output from the query we
	// send to the local package-manager:
	//
	// {PkgName},{PkgVersion}\n
	// ...
	// {PkgName},{PkgVersion}\n
	//
	// first, trim the last carriage return
	managerQueryOut := strings.TrimSuffix(string(managerQuery), "\n")
	// then, split by carriage return
	for _, pkg := range strings.Split(managerQueryOut, "\n") {
		// finally, split by comma to get PackageName and PackageVersion
		pkgDetail := strings.Split(pkg, ",")

		// the splitted package detail must be size of 2 elements
		if len(pkgDetail) != 2 {
			c.Log.Warnw("unable to parse package, expected length=2, skipping",
				"raw_pkg_details", pkg,
				"split_pkg_details", pkgDetail,
			)
			continue
		}

		manifest.OsPkgInfoList = append(manifest.OsPkgInfoList,
			api.OsPkgInfo{
				Os:     osInfo.Name,
				OsVer:  osInfo.Version,
				Pkg:    pkgDetail[0],
				PkgVer: pkgDetail[1],
			},
		)
	}

	c.Event.AddFeatureField("total_manifest_pkgs", len(manifest.OsPkgInfoList))
	c.Log.Debugw("package-manifest", "raw", manifest)
	return c.removeInactivePackagesFromManifest(manifest, manager), nil
}

func (c *cliState) removeInactivePackagesFromManifest(manifest *api.PackageManifest, manager string) *api.PackageManifest {
	// Detect Active Kernel
	//
	// The default behavior of most linux distros is to keep the last N kernel packages
	// installed for users that need to fallback in case the new kernel do not boot.
	// However, the presence of the package does not mean that kernel is active.
	// We must continue to allow the standard kernel package preservation behavior
	// without providing false-positives of vulnerabilities that are not active.
	//
	// We will try to detect the active kernel and remove any other installed-inactive
	// kernel from the generated package manifest
	activeKernel, detected := c.detectActiveKernel()
	c.Event.AddFeatureField("active_kernel", activeKernel)
	if !detected {
		return manifest
	}

	newManifest := new(api.PackageManifest)
	for i, pkg := range manifest.OsPkgInfoList {

		switch manager {
		case "rpm":
			kernelPkgName := "kernel"
			pkgVer := removeEpochFromPkgVersion(pkg.PkgVer)
			if pkg.Pkg == kernelPkgName && !strings.Contains(activeKernel, pkgVer) {
				// this package is NOT the active kernel
				c.Log.Warnw("inactive kernel package detected, removing from generated pkg manifest",
					"pkg_name", kernelPkgName,
					"pkg_version", pkg.PkgVer,
					"active_kernel", activeKernel,
				)
				c.Event.AddFeatureField(
					fmt.Sprintf("kernel_suppressed_%d", i),
					fmt.Sprintf("%s-%s", pkg.Pkg, pkg.PkgVer))
				continue
			}
		case "dpkg-query":
			kernelPkgName := "linux-image-"
			if strings.Contains(pkg.Pkg, kernelPkgName) {
				// this is a kernel package, trim the package name prefix to get the version
				kernelVer := strings.TrimPrefix(pkg.Pkg, kernelPkgName)

				if !strings.Contains(activeKernel, kernelVer) {
					// this package is NOT the active kernel
					c.Log.Warnw("inactive kernel package detected, removing from generated pkg manifest",
						"pkg_name", kernelPkgName,
						"pkg_version", pkg.PkgVer,
						"active_kernel", activeKernel,
					)
					c.Event.AddFeatureField(
						fmt.Sprintf("kernel_suppressed_%d", i),
						fmt.Sprintf("%s-%s", pkg.Pkg, pkg.PkgVer))
					continue
				}
			}
		}

		newManifest.OsPkgInfoList = append(newManifest.OsPkgInfoList, pkg)
	}

	if len(manifest.OsPkgInfoList) != len(newManifest.OsPkgInfoList) {
		c.Log.Debugw("package-manifest modified", "raw", newManifest)
	}
	return newManifest
}

func (c *cliState) detectActiveKernel() (string, bool) {
	kernel, err := exec.Command("uname", "-r").Output()
	if err != nil {
		c.Log.Warnw("unable to detect active kernel",
			"cmd", "uname -r",
			"error", err,
		)
		return "", false
	}
	return strings.TrimSuffix(string(kernel), "\n"), true
}

func (c *cliState) GetOSInfo() (*OS, error) {
	osInfo := new(OS)

	c.Log.Debugw("detecting operating system information",
		"os", runtime.GOOS,
		"arch", runtime.GOARCH,
	)

	f, err := os.Open(osReleaseFile)
	if err != nil {
		msg := `unsupported platform

For more information about supported platforms, visit:
    https://support.lacework.com/hc/en-us/articles/360049666194-Host-Vulnerability-Assessment-Overview`
		return osInfo, errors.New(msg)
	}
	defer f.Close()

	c.Log.Debugw("parsing os release file", "file", osReleaseFile)
	s := bufio.NewScanner(f)
	for s.Scan() {
		if m := rexNameFromID.FindStringSubmatch(s.Text()); m != nil {
			osInfo.Name = strings.Trim(m[1], `"`)
		} else if m := rexVersionID.FindStringSubmatch(s.Text()); m != nil {
			osInfo.Version = strings.Trim(m[1], `"`)
		}
	}

	return osInfo, nil
}

func (c *cliState) DetectPackageManager() (string, error) {
	c.Log.Debugw("detecting package-manager")

	for _, manager := range SupportedPackageManagers {
		if c.checkPackageManager(manager) {
			c.Log.Debugw("detected", "package-manager", manager)
			return manager, nil
		}
	}
	msg := "unable to find supported package managers."
	msg = fmt.Sprintf("%s Supported package managers are %s.",
		msg, strings.Join(SupportedPackageManagers, ", "))
	return "", errors.New(msg)
}

func (c *cliState) checkPackageManager(manager string) bool {
	var (
		cmd    = exec.Command("which", manager)
		_, err = cmd.CombinedOutput()
	)
	if err != nil {
		c.Log.Debugw("error trying to check package-manager",
			"cmd", "which",
			"package-manager", manager,
			"error", err,
		)
		if exitError, ok := err.(*exec.ExitError); ok {
			waitStatus := exitError.Sys().(syscall.WaitStatus)
			return waitStatus.ExitStatus() == 0
		}
		c.Log.Warnw("something went wrong with 'which', trying native command")
		return c.checkPackageManagerWithNativeCommand(manager)
	}
	waitStatus := cmd.ProcessState.Sys().(syscall.WaitStatus)
	return waitStatus.ExitStatus() == 0
}

func (c *cliState) checkPackageManagerWithNativeCommand(manager string) bool {
	var (
		cmd    = exec.Command("command", "-v", manager)
		_, err = cmd.CombinedOutput()
	)
	if err != nil {
		c.Log.Debugw("error trying to check package-manager",
			"cmd", "command",
			"package-manager", manager,
			"error", err,
		)
		if exitError, ok := err.(*exec.ExitError); ok {
			waitStatus := exitError.Sys().(syscall.WaitStatus)
			return waitStatus.ExitStatus() == 0
		}
		return false
	}
	waitStatus := cmd.ProcessState.Sys().(syscall.WaitStatus)
	return waitStatus.ExitStatus() == 0
}

func removeEpochFromPkgVersion(pkgVer string) string {
	if strings.Contains(pkgVer, ":") {
		pkgVerSplit := strings.Split(pkgVer, ":")
		if len(pkgVerSplit) == 2 {
			return pkgVerSplit[1]
		}
	}

	return pkgVer
}
