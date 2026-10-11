package cli

import (
	"bufio"
	"os"
	"sync"
	"time"

	"github.com/spf13/cobra"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/kube"
	"github.com/USA-RedDragon/b2-backup-operator/internal/cli/runner"

	b2backupv1 "github.com/USA-RedDragon/b2-backup-operator/api/v1"
)

// Factory builds the Kubernetes clients the CLI commands share, lazily and once.
type Factory struct {
	ConfigFlags *genericclioptions.ConfigFlags
	Streams     genericiooptions.IOStreams

	allNamespaces bool

	podStartTimeout time.Duration

	stdinOnce sync.Once
	stdin     *bufio.Reader

	interactive *bool

	once struct {
		scheme     sync.Once
		restConfig sync.Once
		client     sync.Once
		clientset  sync.Once
	}
	scheme     *runtime.Scheme
	restConfig *rest.Config
	restErr    error
	client     client.Client
	clientErr  error
	clientset  *kubernetes.Clientset
	csErr      error
}

// NewFactory returns a Factory using the given I/O streams.
func NewFactory(streams genericiooptions.IOStreams) *Factory {
	return &Factory{
		ConfigFlags: genericclioptions.NewConfigFlags(true),
		Streams:     streams,
	}
}

// Scheme returns the runtime scheme, including the b2backup types.
func (f *Factory) Scheme() *runtime.Scheme {
	f.once.scheme.Do(func() {
		s := runtime.NewScheme()
		utilruntime.Must(corev1.AddToScheme(s))
		utilruntime.Must(batchv1.AddToScheme(s))
		utilruntime.Must(b2backupv1.AddToScheme(s))
		f.scheme = s
	})
	return f.scheme
}

// RESTConfig returns the client config from the kubeconfig and flags.
func (f *Factory) RESTConfig() (*rest.Config, error) {
	f.once.restConfig.Do(func() {
		f.restConfig, f.restErr = f.ConfigFlags.ToRESTConfig()
	})
	return f.restConfig, f.restErr
}

// Client returns a controller-runtime client.
func (f *Factory) Client() (client.Client, error) {
	f.once.client.Do(func() {
		cfg, err := f.RESTConfig()
		if err != nil {
			f.clientErr = err
			return
		}
		f.client, f.clientErr = client.New(cfg, client.Options{Scheme: f.Scheme()})
	})
	return f.client, f.clientErr
}

// Clientset returns a client-go clientset.
func (f *Factory) Clientset() (*kubernetes.Clientset, error) {
	f.once.clientset.Do(func() {
		cfg, err := f.RESTConfig()
		if err != nil {
			f.csErr = err
			return
		}
		f.clientset, f.csErr = kubernetes.NewForConfig(cfg)
	})
	return f.clientset, f.csErr
}

// Namespace returns the namespace to act on.
func (f *Factory) Namespace() (string, error) {
	ns, _, err := f.ConfigFlags.ToRawKubeConfigLoader().Namespace()
	return ns, err
}

// ListNamespace returns the namespace to list in, or empty for all namespaces.
func (f *Factory) ListNamespace() (string, error) {
	if f.allNamespaces {
		return "", nil
	}
	return f.Namespace()
}

// AllNamespaces reports whether --all-namespaces was given.
func (f *Factory) AllNamespaces() bool { return f.allNamespaces }

// AddAllNamespacesFlag registers the --all-namespaces flag on cmd.
func (f *Factory) AddAllNamespacesFlag(cmd *cobra.Command) {
	cmd.Flags().BoolVarP(&f.allNamespaces, "all-namespaces", "A", false,
		"List the requested objects across all namespaces")
}

// AddPodStartTimeoutFlag registers the --pod-start-timeout flag on cmd.
func (f *Factory) AddPodStartTimeoutFlag(cmd *cobra.Command) {
	cmd.Flags().DurationVar(&f.podStartTimeout, "pod-start-timeout",
		runner.DefaultPodStartTimeout, "How long to wait for the pod to start running")
}

// Stdin returns a buffered reader over the input stream.
func (f *Factory) Stdin() *bufio.Reader {
	f.stdinOnce.Do(func() { f.stdin = bufio.NewReader(f.Streams.In) })
	return f.stdin
}

// Interactive reports whether the CLI is attached to a terminal.
func (f *Factory) Interactive() bool {
	if f.interactive != nil {
		return *f.interactive
	}
	file, ok := f.Streams.In.(*os.File)
	return ok && kube.IsTerminal(file.Fd())
}
