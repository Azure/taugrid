package cli

import "gopkg.in/yaml.v3"

type offloadAgentDeploymentManifestOptions struct {
	Name           string
	Namespace      string
	AppName        string
	ServiceAccount string
	ContainerName  string
	Image          string
	Args           []string
	CPURequest     string
	MemoryRequest  string
	CPULimit       string
	MemoryLimit    string
	VolumeName     string
	MountPath      string
	PVC            string
	NodeSelector   map[string]string
}

func renderOffloadAgentDeploymentManifest(opts offloadAgentDeploymentManifestOptions) (string, error) {
	labels := map[string]string{"app.kubernetes.io/name": opts.AppName}
	deployment := agentDeploymentManifest{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Metadata: agentObjectMeta{
			Name:      opts.Name,
			Namespace: opts.Namespace,
			Labels:    labels,
		},
		Spec: agentDeploymentSpec{
			Replicas: 1,
			Selector: agentLabelSelector{
				MatchLabels: labels,
			},
			Template: agentPodTemplateSpec{
				Metadata: agentObjectMeta{
					Labels: labels,
				},
				Spec: agentPodSpec{
					ServiceAccountName: opts.ServiceAccount,
					Containers: []agentContainer{{
						Name:            opts.ContainerName,
						Image:           opts.Image,
						ImagePullPolicy: "IfNotPresent",
						Env: []agentEnvVar{{
							Name: "NODE_IP",
							ValueFrom: &agentEnvVarSource{
								FieldRef: &agentObjectFieldSelector{FieldPath: "status.hostIP"},
							},
						}},
						Args: opts.Args,
						Resources: agentResourceRequirements{
							Requests: map[string]string{
								"cpu":    opts.CPURequest,
								"memory": opts.MemoryRequest,
							},
							Limits: map[string]string{
								"cpu":    opts.CPULimit,
								"memory": opts.MemoryLimit,
							},
						},
						SecurityContext: agentSecurityContext{
							AllowPrivilegeEscalation: false,
							ReadOnlyRootFilesystem:   true,
							RunAsNonRoot:             true,
							Capabilities: agentCapabilities{
								Drop: []string{"ALL"},
							},
						},
						VolumeMounts: []agentVolumeMount{
							{Name: "tmp", MountPath: "/tmp"},
							{Name: opts.VolumeName, MountPath: opts.MountPath},
						},
					}},
					NodeSelector: opts.NodeSelector,
					Volumes: []agentVolume{
						{Name: "tmp", EmptyDir: &agentEmptyDirVolumeSource{}},
						{Name: opts.VolumeName, PersistentVolumeClaim: &agentPersistentVolumeClaimVolumeSource{ClaimName: opts.PVC}},
					},
				},
			},
		},
	}
	raw, err := yaml.Marshal(deployment)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

type agentDeploymentManifest struct {
	APIVersion string              `yaml:"apiVersion"`
	Kind       string              `yaml:"kind"`
	Metadata   agentObjectMeta     `yaml:"metadata"`
	Spec       agentDeploymentSpec `yaml:"spec"`
}

type agentObjectMeta struct {
	Name      string            `yaml:"name,omitempty"`
	Namespace string            `yaml:"namespace,omitempty"`
	Labels    map[string]string `yaml:"labels,omitempty"`
}

type agentDeploymentSpec struct {
	Replicas int                  `yaml:"replicas"`
	Selector agentLabelSelector   `yaml:"selector"`
	Template agentPodTemplateSpec `yaml:"template"`
}

type agentLabelSelector struct {
	MatchLabels map[string]string `yaml:"matchLabels"`
}

type agentPodTemplateSpec struct {
	Metadata agentObjectMeta `yaml:"metadata"`
	Spec     agentPodSpec    `yaml:"spec"`
}

type agentPodSpec struct {
	ServiceAccountName string            `yaml:"serviceAccountName"`
	Containers         []agentContainer  `yaml:"containers"`
	NodeSelector       map[string]string `yaml:"nodeSelector,omitempty"`
	Volumes            []agentVolume     `yaml:"volumes"`
}

type agentContainer struct {
	Name            string                    `yaml:"name"`
	Image           string                    `yaml:"image"`
	ImagePullPolicy string                    `yaml:"imagePullPolicy"`
	Env             []agentEnvVar             `yaml:"env"`
	Args            []string                  `yaml:"args"`
	Resources       agentResourceRequirements `yaml:"resources"`
	SecurityContext agentSecurityContext      `yaml:"securityContext"`
	VolumeMounts    []agentVolumeMount        `yaml:"volumeMounts"`
}

type agentEnvVar struct {
	Name      string             `yaml:"name"`
	ValueFrom *agentEnvVarSource `yaml:"valueFrom,omitempty"`
}

type agentEnvVarSource struct {
	FieldRef *agentObjectFieldSelector `yaml:"fieldRef,omitempty"`
}

type agentObjectFieldSelector struct {
	FieldPath string `yaml:"fieldPath"`
}

type agentResourceRequirements struct {
	Requests map[string]string `yaml:"requests"`
	Limits   map[string]string `yaml:"limits"`
}

type agentSecurityContext struct {
	AllowPrivilegeEscalation bool              `yaml:"allowPrivilegeEscalation"`
	ReadOnlyRootFilesystem   bool              `yaml:"readOnlyRootFilesystem"`
	RunAsNonRoot             bool              `yaml:"runAsNonRoot"`
	Capabilities             agentCapabilities `yaml:"capabilities"`
}

type agentCapabilities struct {
	Drop []string `yaml:"drop"`
}

type agentVolumeMount struct {
	Name      string `yaml:"name"`
	MountPath string `yaml:"mountPath"`
}

type agentVolume struct {
	Name                  string                                  `yaml:"name"`
	EmptyDir              *agentEmptyDirVolumeSource              `yaml:"emptyDir,omitempty"`
	PersistentVolumeClaim *agentPersistentVolumeClaimVolumeSource `yaml:"persistentVolumeClaim,omitempty"`
}

type agentEmptyDirVolumeSource struct{}

type agentPersistentVolumeClaimVolumeSource struct {
	ClaimName string `yaml:"claimName"`
}
