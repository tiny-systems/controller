/*
One binary, two roles:

	tiny-controller manager   — the hands: reconciles Sessions into pods and
	                            carries out allowed Question actions. Runs
	                            once, holds the RBAC.
	tiny-controller serve     — the voice: the MCP toolbox (ask_human,
	                            session_list, session_create, expose_port).
	                            Runs as a localhost sidecar in every session
	                            pod, powerless beyond writing Questions; or
	                            standalone for foreign runners.

The split is the security model: agents talk to a sidecar that can only ask;
the single manager is the only identity that can act.
*/
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	agentsv1 "github.com/tiny-systems/controller/api/v1alpha1"
	"github.com/tiny-systems/controller/internal/controller"
	"github.com/tiny-systems/controller/internal/tools"
)

func main() {
	role := "manager"
	args := os.Args[1:]
	if len(args) > 0 && (args[0] == "serve" || args[0] == "manager") {
		role = args[0]
		args = args[1:]
	}
	switch role {
	case "serve":
		serve(args)
	default:
		manager(args)
	}
}

func scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(agentsv1.AddToScheme(s))
	return s
}

// serve runs the MCP toolbox: localhost sidecar in a session pod, or a
// standalone endpoint for anything else that wants the gate.
func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var addr, namespace string
	fs.StringVar(&addr, "addr", ":8080", "listen address for /mcp, /attention and /healthz")
	fs.StringVar(&namespace, "namespace", os.Getenv("POD_NAMESPACE"), "namespace Questions live in (default: POD_NAMESPACE)")
	_ = fs.Parse(args)
	if namespace == "" {
		namespace = "default"
	}

	cfg, err := ctrl.GetConfig()
	if err != nil {
		log.Fatalf("kubeconfig: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme()})
	if err != nil {
		log.Fatalf("cluster client: %v", err)
	}

	srv := &tools.Server{Client: c, Namespace: namespace}
	log.Printf("tiny-mcp serving on %s (namespace %s): /mcp, /attention, /healthz", addr, namespace)
	s := &http.Server{
		Addr:    addr,
		Handler: srv.Handler(),
		// ask_human blocks by design; only reads are bounded.
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(s.ListenAndServe())
}

// manager runs the reconcilers.
func manager(args []string) {
	fs := flag.NewFlagSet("manager", flag.ExitOnError)
	var metricsAddr, probeAddr, agentImage, sidecarImage string
	var enableLeaderElection bool
	fs.StringVar(&metricsAddr, "metrics-bind-address", "0", "metrics endpoint ('0' disables)")
	fs.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "health probe endpoint")
	fs.BoolVar(&enableLeaderElection, "leader-elect", false, "enable leader election")
	fs.StringVar(&agentImage, "agent-image", "ghcr.io/tiny-systems/agent:latest", "default coding-agent image for sessions")
	fs.StringVar(&sidecarImage, "sidecar-image", "ghcr.io/tiny-systems/controller:latest", "tiny-mcp sidecar image for sessions")
	var watchNamespace string
	fs.StringVar(&watchNamespace, "namespace", os.Getenv("POD_NAMESPACE"),
		"namespace to watch (default: POD_NAMESPACE). Namespace-scoped by design — a plain Role suffices; empty watches the cluster and needs a ClusterRole")
	_ = fs.Parse(args)

	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))

	opts := ctrl.Options{
		Scheme:                 scheme(),
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "tiny-controller.tinysystems.io",
	}
	if watchNamespace != "" {
		opts.Cache = cache.Options{DefaultNamespaces: map[string]cache.Config{watchNamespace: {}}}
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), opts)
	if err != nil {
		log.Fatalf("manager: %v", err)
	}

	if err := (&controller.SessionReconciler{
		Client: mgr.GetClient(),
		Images: controller.Images{Agent: agentImage, Sidecar: sidecarImage},
	}).SetupWithManager(mgr); err != nil {
		log.Fatalf("session controller: %v", err)
	}
	if err := (&controller.QuestionReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		log.Fatalf("question controller: %v", err)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Fatalf("healthz: %v", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Fatalf("readyz: %v", err)
	}

	log.Print("tiny-controller manager starting")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Fatalf("manager exited: %v", err)
	}
}
