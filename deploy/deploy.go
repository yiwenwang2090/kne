package deploy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	dtypes "github.com/docker/docker/api/types"
	dclient "github.com/docker/docker/client"
	"github.com/openconfig/gnmi/errlist"
	kexec "github.com/openconfig/kne/os/exec"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	dockerConfigEnvVar           = "DOCKER_CONFIG"
	kubeletConfigPathTemplate    = "%s:/var/lib/kubelet/config.json"
	dockerConfigTemplateContents = `{
  "auths": {
{{range $val := .}}    "{{$val}}": {}
{{end}}  }
}
`
	ixiaTGConfigMapHeader = `apiVersion: v1
kind: ConfigMap
metadata:
  name: ixiatg-release-config
  namespace: ixiatg-op-system
data:
  versions: |
    `
)

var (
	dockerConfigTemplate = template.Must(template.New("dockerConfig").Parse(dockerConfigTemplateContents))
	logOut               = log.StandardLogger().Out
	healthTimeout        = time.Minute

	// execer handles all execs on host.
	execer execerInterface = kexec.NewExecer(logOut, logOut)

	// Stubs for testing.
	execLookPath = exec.LookPath
	osStat       = os.Stat
)

type execerInterface interface {
	Exec(string, ...string) error
	SetStdout(io.Writer)
	SetStderr(io.Writer)
}

type Cluster interface {
	Deploy(context.Context) error
	Delete() error
	Healthy() error
	GetName() string
}

type Ingress interface {
	Deploy(context.Context) error
	SetKClient(kubernetes.Interface)
	Healthy(context.Context) error
}

type CNI interface {
	Deploy(context.Context) error
	SetKClient(kubernetes.Interface)
	Healthy(context.Context) error
}

type Controller interface {
	Deploy(context.Context) error
	SetKClient(kubernetes.Interface)
	Healthy(context.Context) error
}

type Deployment struct {
	Cluster     Cluster
	Ingress     Ingress
	CNI         CNI
	Controllers []Controller
}

func (d *Deployment) String() string {
	b, _ := json.MarshalIndent(d, "", "\t")
	return string(b)
}

func (d *Deployment) checkDependencies() error {
	var errs errlist.List
	for _, bin := range []string{"docker", "kubectl"} {
		if _, err := execLookPath(bin); err != nil {
			errs.Add(fmt.Errorf("install dependency %q to deploy", bin))
		}
	}
	return errs.Err()
}

func (d *Deployment) Deploy(ctx context.Context, kubecfg string) error {
	if err := d.checkDependencies(); err != nil {
		return err
	}
	log.Infof("Deploying cluster...")
	if err := d.Cluster.Deploy(ctx); err != nil {
		return err
	}
	log.Infof("Cluster deployed")
	// Once cluster is up, set kClient
	rCfg, err := clientcmd.BuildConfigFromFlags("", kubecfg)
	if err != nil {
		return err
	}
	kClient, err := kubernetes.NewForConfig(rCfg)
	if err != nil {
		return err
	}
	d.Ingress.SetKClient(kClient)
	log.Infof("Deploying ingress...")
	if err := d.Ingress.Deploy(ctx); err != nil {
		return err
	}
	tCtx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	if err := d.Ingress.Healthy(tCtx); err != nil {
		return err
	}
	log.Infof("Ingress healthy")
	log.Infof("Deploying CNI...")
	if err := d.CNI.Deploy(ctx); err != nil {
		return err
	}
	d.CNI.SetKClient(kClient)
	tCtx, cancel = context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	if err := d.CNI.Healthy(tCtx); err != nil {
		return err
	}
	log.Infof("CNI healthy")
	for _, c := range d.Controllers {
		log.Infof("Deploying controller...")
		if err := c.Deploy(ctx); err != nil {
			return err
		}
		c.SetKClient(kClient)
		tCtx, cancel = context.WithTimeout(ctx, healthTimeout)
		defer cancel()
		if err := c.Healthy(tCtx); err != nil {
			return err
		}
	}
	log.Infof("Controllers deployed and healthy")
	return nil
}

func (d *Deployment) Delete() error {
	log.Infof("Deleting cluster...")
	if err := d.Cluster.Delete(); err != nil {
		return err
	}
	log.Infof("Cluster deleted")
	return nil
}

func (d *Deployment) Healthy(ctx context.Context) error {
	if err := d.Cluster.Healthy(); err != nil {
		return err
	}
	log.Infof("Cluster healthy")
	tCtx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	if err := d.Ingress.Healthy(tCtx); err != nil {
		return err
	}
	log.Infof("Ingress healthy")
	tCtx, cancel = context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	if err := d.CNI.Healthy(tCtx); err != nil {
		return err
	}
	log.Infof("CNI healthy")
	for _, c := range d.Controllers {
		tCtx, cancel = context.WithTimeout(ctx, healthTimeout)
		defer cancel()
		if err := c.Healthy(tCtx); err != nil {
			return err
		}
	}
	log.Infof("Controllers healthy")
	return nil
}

type KindSpec struct {
	Name                     string            `yaml:"name"`
	Recycle                  bool              `yaml:"recycle"`
	Version                  string            `yaml:"version"`
	Image                    string            `yaml:"image"`
	Retain                   bool              `yaml:"retain"`
	Wait                     time.Duration     `yaml:"wait"`
	Kubecfg                  string            `yaml:"kubecfg"`
	GoogleArtifactRegistries []string          `yaml:"googleArtifactRegistries"`
	ContainerImages          map[string]string `yaml:"containerImages"`
	KindConfigFile           string            `yaml:"config"`
	AdditionalManifests      []string          `yaml:"additionalManifests"`
}

func (k *KindSpec) checkDependencies() error {
	var errs errlist.List
	bins := []string{"kind"}
	if len(k.GoogleArtifactRegistries) != 0 {
		bins = append(bins, "gcloud")
	}
	for _, bin := range bins {
		if _, err := execLookPath(bin); err != nil {
			errs.Add(fmt.Errorf("install dependency %q to deploy", bin))
		}
	}
	return errs.Err()
}

func (k *KindSpec) Deploy(ctx context.Context) error {
	if err := k.checkDependencies(); err != nil {
		return err
	}
	if k.Recycle {
		log.Infof("Attempting to recycle existing cluster %q...", k.Name)
		if err := execer.Exec("kubectl", "cluster-info", "--context", fmt.Sprintf("kind-%s", k.Name)); err == nil {
			log.Infof("Recycling existing cluster %q", k.Name)
			return nil
		}
	}
	args := []string{"create", "cluster"}
	if k.Name != "" {
		args = append(args, "--name", k.Name)
	}
	if k.Image != "" {
		args = append(args, "--image", k.Image)
	}
	if k.Retain {
		args = append(args, "--retain")
	}
	if k.Wait != 0 {
		args = append(args, "--wait", k.Wait.String())
	}
	if k.Kubecfg != "" {
		args = append(args, "--kubeconfig", k.Kubecfg)
	}
	if k.KindConfigFile != "" {
		args = append(args, "--config", k.KindConfigFile)
	}
	log.Infof("Creating kind cluster with: %v", args)
	if err := execer.Exec("kind", args...); err != nil {
		return fmt.Errorf("failed to create cluster: %w", err)
	}
	log.Infof("Deployed kind cluster: %s", k.Name)
	for _, s := range k.AdditionalManifests {
		log.Infof("Found manifest %q", s)
		if err := execer.Exec("kubectl", "apply", "-f", s); err != nil {
			return fmt.Errorf("failed to deploy manifest: %w", err)
		}
	}
	if len(k.GoogleArtifactRegistries) != 0 {
		log.Infof("Setting up Google Artifact Registry access for %v", k.GoogleArtifactRegistries)
		if err := k.setupGoogleArtifactRegistryAccess(); err != nil {
			return fmt.Errorf("failed to setup Google artifact registry access: %w", err)
		}
	}
	if len(k.ContainerImages) != 0 {
		log.Infof("Loading container images")
		if err := k.loadContainerImages(); err != nil {
			return fmt.Errorf("failed to load container images: %w", err)
		}
	}
	return nil
}

func (k *KindSpec) Delete() error {
	args := []string{"delete", "cluster"}
	if k.Name != "" {
		args = append(args, "--name", k.Name)
	}
	if err := execer.Exec("kind", args...); err != nil {
		return fmt.Errorf("failed to delete cluster using cli: %w", err)
	}
	return nil
}

func (k *KindSpec) Healthy() error {
	if err := execer.Exec("kubectl", "cluster-info", "--context", fmt.Sprintf("kind-%s", k.GetName())); err != nil {
		return fmt.Errorf("cluster not healthy: %w", err)
	}
	return nil
}

func (k *KindSpec) GetName() string {
	if k.Name != "" {
		return k.Name
	}
	return "kind"
}

func (k *KindSpec) setupGoogleArtifactRegistryAccess() error {
	// Create a temporary dir to hold a new docker config that lacks credsStore.
	// Then use `docker login` to store the generated credentials directly in
	// the temporary docker config.
	// See https://kind.sigs.k8s.io/docs/user/private-registries/#use-an-access-token
	// for more information.
	tempDockerDir, err := os.MkdirTemp("", "kne_kind_docker")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDockerDir)
	originalConfig := os.Getenv(dockerConfigEnvVar)
	defer os.Setenv(dockerConfigEnvVar, originalConfig)
	if err := os.Setenv(dockerConfigEnvVar, tempDockerDir); err != nil {
		return err
	}
	configPath := filepath.Join(tempDockerDir, "config.json")
	if err := writeDockerConfig(configPath, k.GoogleArtifactRegistries); err != nil {
		return err
	}
	var token bytes.Buffer
	execer.SetStdout(&token)
	if err := execer.Exec("gcloud", "auth", "print-access-token"); err != nil {
		return err
	}
	execer.SetStdout(log.StandardLogger().Out)
	for _, r := range k.GoogleArtifactRegistries {
		s := fmt.Sprintf("https://%s", r)
		if err := execer.Exec("docker", "login", "-u", "oauth2accesstoken", "-p", token.String(), s); err != nil {
			return err
		}
	}
	args := []string{"get", "nodes"}
	if k.Name != "" {
		args = append(args, "--name", k.Name)
	}
	var nodes bytes.Buffer
	execer.SetStdout(&nodes)
	if err := execer.Exec("kind", args...); err != nil {
		return err
	}
	execer.SetStdout(log.StandardLogger().Out)
	// Copy the new docker config to each node and restart kubelet so it
	// picks up the new config that contains the embedded credentials.
	for _, node := range strings.Split(nodes.String(), " ") {
		node = strings.TrimSuffix(node, "\n")
		if err := execer.Exec("docker", "cp", configPath, fmt.Sprintf(kubeletConfigPathTemplate, node)); err != nil {
			return err
		}
		if err := execer.Exec("docker", "exec", node, "systemctl", "restart", "kubelet.service"); err != nil {
			return err
		}
	}
	log.Infof("Setup credentials for accessing GAR locations %v in kind cluster", k.GoogleArtifactRegistries)
	return nil
}

func (k *KindSpec) loadContainerImages() error {
	for s, d := range k.ContainerImages {
		log.Infof("Loading %q as %q", s, d)
		if err := execer.Exec("docker", "pull", s); err != nil {
			return fmt.Errorf("failed to pull %q: %w", s, err)
		}
		if err := execer.Exec("docker", "tag", s, d); err != nil {
			return fmt.Errorf("failed to tag %q with %q: %w", s, d, err)
		}
		args := []string{"load", "docker-image", d}
		if k.Name != "" {
			args = append(args, "--name", k.Name)
		}
		if err := execer.Exec("kind", args...); err != nil {
			return fmt.Errorf("failed to load %q: %w", d, err)
		}
	}
	log.Infof("Loaded all container images")
	return nil
}

func writeDockerConfig(path string, registries []string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return dockerConfigTemplate.Execute(f, registries)
}

type MetalLBSpec struct {
	Version     string `yaml:"version"`
	IPCount     int    `yaml:"ip_count"`
	ManifestDir string `yaml:"manifests"`
	kClient     kubernetes.Interface
	dClient     dclient.NetworkAPIClient
}

func (m *MetalLBSpec) SetKClient(c kubernetes.Interface) {
	m.kClient = c
}

func inc(ip net.IP, cnt int) {
	for cnt > 0 {
		for j := len(ip) - 1; j >= 0; j-- {
			ip[j]++
			if ip[j] > 0 {
				break
			}
		}
		cnt--
	}
}

type pool struct {
	Name      string   `yaml:"name"`
	Protocol  string   `yaml:"protocol"`
	Addresses []string `yaml:"addresses"`
}

type metalLBConfig struct {
	AddressPools []pool `yaml:"address-pools"`
}

func makeConfig(n *net.IPNet, count int) metalLBConfig {
	start := make(net.IP, len(n.IP))
	copy(start, n.IP)
	inc(start, 50)
	end := make(net.IP, len(start))
	copy(end, start)
	inc(end, count)
	return metalLBConfig{
		AddressPools: []pool{{
			Name:      "default",
			Protocol:  "layer2",
			Addresses: []string{fmt.Sprintf("%s - %s", start, end)},
		}},
	}
}

func (m *MetalLBSpec) Deploy(ctx context.Context) error {
	if m.dClient == nil {
		var err error
		m.dClient, err = dclient.NewClientWithOpts(dclient.FromEnv)
		if err != nil {
			return err
		}
	}
	log.Infof("Creating metallb namespace")
	if err := execer.Exec("kubectl", "apply", "-f", filepath.Join(m.ManifestDir, "namespace.yaml")); err != nil {
		return err
	}
	_, err := m.kClient.CoreV1().Secrets("metallb-system").Get(ctx, "memberlist", metav1.GetOptions{})
	if err != nil {
		log.Infof("Creating metallb secret")
		d := make([]byte, 16)
		rand.Read(d)
		s := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: "memberlist",
			},
			StringData: map[string]string{
				"secretkey": base64.StdEncoding.EncodeToString(d),
			},
		}
		_, err := m.kClient.CoreV1().Secrets("metallb-system").Create(ctx, s, metav1.CreateOptions{})
		if err != nil {
			return err
		}
	}
	log.Infof("Applying metallb pods")
	if err := execer.Exec("kubectl", "apply", "-f", filepath.Join(m.ManifestDir, "metallb.yaml")); err != nil {
		return err
	}
	_, err = m.kClient.CoreV1().ConfigMaps("metallb-system").Get(ctx, "config", metav1.GetOptions{})
	if err != nil {
		log.Infof("Applying metallb ingress config")
		// Get Network information from docker.
		nr, err := m.dClient.NetworkList(ctx, dtypes.NetworkListOptions{})
		if err != nil {
			return err
		}
		var network dtypes.NetworkResource
		for _, v := range nr {
			if v.Name == "kind" {
				network = v
				break
			}
		}
		var n *net.IPNet
		for _, ipRange := range network.IPAM.Config {
			_, ipNet, err := net.ParseCIDR(ipRange.Subnet)
			if err != nil {
				return err
			}
			if ipNet.IP.To4() != nil {
				n = ipNet
				break
			}
		}
		if n == nil {
			return fmt.Errorf("failed to find kind ipv4 docker net")
		}
		config := makeConfig(n, m.IPCount)
		b, err := yaml.Marshal(config)
		if err != nil {
			return err
		}
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: "config",
			},
			Data: map[string]string{
				"config": string(b),
			},
		}
		_, err = m.kClient.CoreV1().ConfigMaps("metallb-system").Create(ctx, cm, metav1.CreateOptions{})
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *MetalLBSpec) Healthy(ctx context.Context) error {
	return deploymentHealthy(ctx, m.kClient, "metallb-system")
}

type MeshnetSpec struct {
	Image       string `yaml:"image"`
	ManifestDir string `yaml:"manifests"`
	kClient     kubernetes.Interface
}

func (m *MeshnetSpec) SetKClient(c kubernetes.Interface) {
	m.kClient = c
}

func (m *MeshnetSpec) Deploy(ctx context.Context) error {
	log.Infof("Deploying Meshnet from: %s", m.ManifestDir)
	if err := execer.Exec("kubectl", "apply", "-k", m.ManifestDir); err != nil {
		return err
	}
	log.Infof("Meshnet Deployed")
	return nil
}

func (m *MeshnetSpec) Healthy(ctx context.Context) error {
	log.Infof("Waiting on Meshnet to be Healthy")
	w, err := m.kClient.AppsV1().DaemonSets("meshnet").Watch(ctx, metav1.ListOptions{
		FieldSelector: fields.SelectorFromSet(fields.Set{metav1.ObjectNameField: "meshnet"}).String(),
	})
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context canceled before healthy")
		case e, ok := <-w.ResultChan():
			if !ok {
				return fmt.Errorf("watch channel closed before healthy")
			}
			d, ok := e.Object.(*appsv1.DaemonSet)
			if !ok {
				return fmt.Errorf("invalid object type: %T", d)
			}
			if d.Status.NumberReady == d.Status.DesiredNumberScheduled &&
				d.Status.NumberUnavailable == 0 {
				log.Infof("Meshnet Healthy")
				return nil
			}
		}
	}
}

type IxiaTGSpec struct {
	ManifestDir string           `yaml:"manifests"`
	ConfigMap   *IxiaTGConfigMap `yaml:"configMap"`
	kClient     kubernetes.Interface
}

type IxiaTGConfigMap struct {
	Release string         `yaml:"release" json:"release"`
	Images  []*IxiaTGImage `yaml:"images" json:"images"`
}

type IxiaTGImage struct {
	Name string `yaml:"name" json:"name"`
	Path string `yaml:"path" json:"path"`
	Tag  string `yaml:"tag" json:"tag"`
}

func (i *IxiaTGSpec) SetKClient(c kubernetes.Interface) {
	i.kClient = c
}

func (i *IxiaTGSpec) Deploy(ctx context.Context) error {
	log.Infof("Deploying IxiaTG controller from: %s", i.ManifestDir)
	if err := execer.Exec("kubectl", "apply", "-f", filepath.Join(i.ManifestDir, "ixiatg-operator.yaml")); err != nil {
		return err
	}
	if i.ConfigMap == nil {
		path := filepath.Join(i.ManifestDir, "ixia-configmap.yaml")
		if _, err := osStat(path); err != nil {
			return fmt.Errorf("ixia configmap not found: %v", err)
		}
		log.Infof("Deploying IxiaTG configmap from: %s", path)
		if err := execer.Exec("kubectl", "apply", "-f", path); err != nil {
			return err
		}
		log.Infof("IxiaTG controller Deployed")
		return nil
	}
	b, err := json.MarshalIndent(i.ConfigMap, "    ", "  ")
	if err != nil {
		return err
	}
	b = append([]byte(ixiaTGConfigMapHeader), b...)
	f, err := os.CreateTemp("", "ixiatg-configmap-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		return err
	}
	log.Infof("Deploying IxiaTG configmap from: %s", f.Name())
	if err := execer.Exec("kubectl", "apply", "-f", f.Name()); err != nil {
		return err
	}
	log.Infof("IxiaTG controller Deployed")
	return nil
}

func (i *IxiaTGSpec) Healthy(ctx context.Context) error {
	return deploymentHealthy(ctx, i.kClient, "ixiatg-op-system")
}

func deploymentHealthy(ctx context.Context, c kubernetes.Interface, name string) error {
	log.Infof("Waiting on deployment %q to be healthy", name)
	w, err := c.AppsV1().Deployments(name).Watch(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	ch := w.ResultChan()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context canceled before healthy")
		case e, ok := <-ch:
			if !ok {
				return fmt.Errorf("watch channel closed before healthy")
			}
			d, ok := e.Object.(*appsv1.Deployment)
			if !ok {
				return fmt.Errorf("invalid object type: %T", d)
			}
			var r int32 = 1
			if d.Spec.Replicas != nil {
				r = *d.Spec.Replicas
			}
			if d.Status.AvailableReplicas == r &&
				d.Status.ReadyReplicas == r &&
				d.Status.UnavailableReplicas == 0 &&
				d.Status.Replicas == r &&
				d.Status.UpdatedReplicas == r {
				log.Infof("Deployment %q healthy", name)
				return nil
			}
		}
	}
}
