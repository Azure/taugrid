// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package clusteraccess

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"k8s.io/client-go/tools/clientcmd"
)

const minimumKubeloginVersion = "0.1.7"

var kubeloginSemanticVersion = regexp.MustCompile(`v?([0-9]+)\.([0-9]+)\.([0-9]+)`)

func (p AKSUserCredentialProvider) resolveKubeloginPath(required bool) (string, error) {
	if path := strings.TrimSpace(p.KubeloginPath); path != "" {
		return path, nil
	}
	findExecutable := p.FindExecutable
	if findExecutable == nil {
		findExecutable = exec.LookPath
	}
	path, err := findExecutable("kubelogin")
	if err == nil {
		return path, nil
	}
	if required {
		return "", fmt.Errorf(
			"kubelogin %s or newer is required for AKS user authentication; install or upgrade it and retry: %w",
			minimumKubeloginVersion,
			err,
		)
	}
	return "", nil
}

func (p AKSUserCredentialProvider) requireCompatibleKubelogin(ctx context.Context, path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf(
			"kubelogin %s or newer is required for this AKS cluster-user kubeconfig; install or upgrade it and retry",
			minimumKubeloginVersion,
		)
	}
	readVersion := p.KubeloginVersion
	if readVersion == nil {
		readVersion = func(ctx context.Context, path string) (string, error) {
			output, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
			if err != nil {
				return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
			}
			return string(output), nil
		}
	}
	output, err := readVersion(ctx, path)
	if err != nil {
		return fmt.Errorf("inspect kubelogin version: %w", err)
	}
	version, parsed, err := parseKubeloginVersion(output)
	if err != nil {
		return err
	}
	minimum := [3]int{0, 1, 7}
	if parsed[0] < minimum[0] ||
		(parsed[0] == minimum[0] && parsed[1] < minimum[1]) ||
		(parsed[0] == minimum[0] && parsed[1] == minimum[1] && parsed[2] < minimum[2]) {
		return fmt.Errorf(
			"kubelogin %s is unsupported; Tau requires kubelogin %s or newer for safe AKS authentication",
			version,
			minimumKubeloginVersion,
		)
	}
	return nil
}

func parseKubeloginVersion(output string) (string, [3]int, error) {
	for _, line := range strings.Split(output, "\n") {
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "kubelogin") && !strings.Contains(lower, "git hash") {
			continue
		}
		match := kubeloginSemanticVersion.FindStringSubmatch(line)
		if len(match) == 0 {
			continue
		}
		var parsed [3]int
		for i := range parsed {
			value, err := strconv.Atoi(match[i+1])
			if err != nil {
				return "", [3]int{}, fmt.Errorf("parse kubelogin version %q: %w", match[0], err)
			}
			parsed[i] = value
		}
		return fmt.Sprintf("%d.%d.%d", parsed[0], parsed[1], parsed[2]), parsed, nil
	}
	return "", [3]int{}, fmt.Errorf(
		"could not determine kubelogin version; Tau requires kubelogin %s or newer for safe AKS authentication",
		minimumKubeloginVersion,
	)
}

func kubeconfigUsesExecCredential(raw []byte) (bool, error) {
	config, err := clientcmd.Load(raw)
	if err != nil {
		return false, fmt.Errorf("inspect normalized AKS kubeconfig: %w", err)
	}
	current := config.Contexts[config.CurrentContext]
	if current == nil {
		return false, fmt.Errorf("normalized AKS kubeconfig has no current context")
	}
	authInfo := config.AuthInfos[current.AuthInfo]
	if authInfo == nil {
		return false, fmt.Errorf("normalized AKS kubeconfig has no user authentication")
	}
	return authInfo.Exec != nil, nil
}
