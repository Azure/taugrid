package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	"github.com/Azure/taugrid/cli/internal/artifactbundle"
)

const azureBlobCSIDriver = "blob.csi.azure.com"

const runBlobVolumeSchema = "tau.run.blob-volume.v1"

type runBlobVolume struct {
	SchemaVersion string `json:"schema_version"`
	AccountURL    string `json:"account_url"`
	Container     string `json:"container"`
}

func resolveRunBlobVolume(ctx context.Context, reader runResultReader, namespace, pvcName string) (runBlobVolume, error) {
	rawPVC, err := reader.Raw(ctx, []string{"get", "pvc", pvcName, "-n", namespace, "-o", "json"}, nil)
	if err != nil {
		return runBlobVolume{}, fmt.Errorf("resolve artifact transport from PVC %s/%s: %w", namespace, pvcName, err)
	}
	var pvc struct {
		Spec struct {
			VolumeName string `json:"volumeName"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(rawPVC), &pvc); err != nil {
		return runBlobVolume{}, fmt.Errorf("decode PVC %s/%s: %w", namespace, pvcName, err)
	}
	if strings.TrimSpace(pvc.Spec.VolumeName) == "" {
		return runBlobVolume{}, fmt.Errorf("PVC %s/%s is not bound to a PersistentVolume", namespace, pvcName)
	}
	rawPV, err := reader.Raw(ctx, []string{"get", "pv", pvc.Spec.VolumeName, "-o", "json"}, nil)
	if err != nil {
		return runBlobVolume{}, fmt.Errorf("resolve artifact transport from PV %s: %w", pvc.Spec.VolumeName, err)
	}
	var pv struct {
		Spec struct {
			CSI struct {
				Driver           string            `json:"driver"`
				VolumeHandle     string            `json:"volumeHandle"`
				VolumeAttributes map[string]string `json:"volumeAttributes"`
			} `json:"csi"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(rawPV), &pv); err != nil {
		return runBlobVolume{}, fmt.Errorf("decode PV %s: %w", pvc.Spec.VolumeName, err)
	}
	if !strings.EqualFold(strings.TrimSpace(pv.Spec.CSI.Driver), azureBlobCSIDriver) {
		return runBlobVolume{}, fmt.Errorf(
			"PVC %s/%s uses CSI driver %q; complete Tau bundle retrieval currently requires %s and will not create a PVC-reader pod",
			namespace, pvcName, pv.Spec.CSI.Driver, azureBlobCSIDriver,
		)
	}
	attributes, err := foldVolumeAttributes(pv.Spec.CSI.VolumeAttributes)
	if err != nil {
		return runBlobVolume{}, fmt.Errorf("PV %s: %w", pvc.Spec.VolumeName, err)
	}
	account := strings.TrimSpace(attributes["storageaccount"])
	containerName := strings.TrimSpace(attributes["containername"])
	parts := strings.Split(pv.Spec.CSI.VolumeHandle, "#")
	if account == "" && len(parts) > 1 {
		account = strings.TrimSpace(parts[1])
	}
	if containerName == "" && len(parts) > 2 {
		containerName = strings.TrimSpace(parts[2])
	}
	if account == "" || containerName == "" {
		return runBlobVolume{}, fmt.Errorf(
			"PV %s does not expose a Blob CSI storageAccount/containerName identity; Tau will not read Secret credentials",
			pvc.Spec.VolumeName,
		)
	}
	server := strings.TrimSpace(attributes["server"])
	if server == "" {
		suffix := firstNonEmpty(attributes["storageendpointsuffix"], "core.windows.net")
		server = account + ".blob." + suffix
	}
	if !strings.Contains(server, "://") {
		server = "https://" + server
	}
	parsed, err := url.Parse(server)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return runBlobVolume{}, fmt.Errorf("PV %s has invalid Blob server %q", pvc.Spec.VolumeName, server)
	}
	return runBlobVolume{
		SchemaVersion: runBlobVolumeSchema,
		AccountURL:    strings.TrimSuffix(parsed.String(), "/"),
		Container:     containerName,
	}, nil
}

func parseRunBlobVolume(raw string) (runBlobVolume, error) {
	var volume runBlobVolume
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&volume); err != nil {
		return runBlobVolume{}, fmt.Errorf("decode Blob artifact transport annotation: %w", err)
	}
	if err := validateRunBlobVolume(volume); err != nil {
		return runBlobVolume{}, err
	}
	if !trustedAzureBlobHost(volume.AccountURL) {
		return runBlobVolume{}, fmt.Errorf("Blob artifact transport account URL %q is not a trusted Azure Blob endpoint", volume.AccountURL)
	}
	return volume, nil
}

func validateRunBlobVolume(volume runBlobVolume) error {
	if volume.SchemaVersion != runBlobVolumeSchema {
		return fmt.Errorf("Blob artifact transport schema %q is unsupported", volume.SchemaVersion)
	}
	parsed, err := url.Parse(strings.TrimSpace(volume.AccountURL))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return fmt.Errorf("Blob artifact transport account URL %q is invalid", volume.AccountURL)
	}
	containerName := strings.TrimSpace(volume.Container)
	if containerName == "" || strings.ContainsAny(containerName, `/\`) {
		return fmt.Errorf("Blob artifact transport container %q is invalid", volume.Container)
	}
	return nil
}

func trustedAzureBlobHost(accountURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(accountURL))
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	for _, suffix := range []string{
		".blob.core.windows.net",
		".blob.core.usgovcloudapi.net",
		".blob.core.chinacloudapi.cn",
		".blob.core.cloudapi.de",
	} {
		if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
			return true
		}
	}
	return false
}

func foldVolumeAttributes(attributes map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(attributes))
	for key, value := range attributes {
		folded := strings.ToLower(strings.TrimSpace(key))
		if previous, exists := out[folded]; exists && previous != value {
			return nil, fmt.Errorf("volumeAttributes contains conflicting case variants for %q", key)
		}
		out[folded] = value
	}
	return out, nil
}

type azureRunArtifactStore struct {
	container *container.Client
}

func newAzureRunArtifactStore(volume runBlobVolume) (*azureRunArtifactStore, error) {
	if err := validateRunBlobVolume(volume); err != nil {
		return nil, err
	}
	if !trustedAzureBlobHost(volume.AccountURL) {
		return nil, fmt.Errorf("Blob artifact transport account URL %q is not a trusted Azure Blob endpoint", volume.AccountURL)
	}
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("create Azure credential for artifact retrieval: %w", err)
	}
	client, err := container.NewClient(volume.AccountURL+"/"+url.PathEscape(volume.Container), credential, nil)
	if err != nil {
		return nil, fmt.Errorf("create Azure Blob artifact client: %w", err)
	}
	return &azureRunArtifactStore{container: client}, nil
}

func (s *azureRunArtifactStore) Read(ctx context.Context, name string) ([]byte, error) {
	var out bytes.Buffer
	if err := s.Download(ctx, name, &out); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (s *azureRunArtifactStore) List(ctx context.Context, prefix string) ([]artifactbundle.Object, error) {
	pager := s.container.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{Prefix: to.Ptr(prefix)})
	var objects []artifactbundle.Object
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		if page.Segment == nil {
			continue
		}
		for _, item := range page.Segment.BlobItems {
			if item == nil || item.Name == nil {
				continue
			}
			size := int64(-1)
			if item.Properties != nil && item.Properties.ContentLength != nil {
				size = *item.Properties.ContentLength
			}
			objects = append(objects, artifactbundle.Object{Name: *item.Name, Size: size})
		}
	}
	return objects, nil
}

func (s *azureRunArtifactStore) Download(ctx context.Context, name string, out io.Writer) error {
	response, err := s.container.NewBlockBlobClient(name).DownloadStream(ctx, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, err = io.Copy(out, response.Body)
	return err
}
