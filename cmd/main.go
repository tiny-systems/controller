/*
The tiny-controller binary serves the human gate: the ask_human MCP endpoint
and the /attention safety net, backed by Question objects in the cluster.

There is deliberately no manager, no leader election, no reconcile loop —
nothing here owns state that needs reconciling. Questions are created by
agents, answered by people, and read by screens; this process is only the
doorway between the first two.
*/
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	tinyv1 "github.com/tiny-systems/controller/api/v1alpha1"
	"github.com/tiny-systems/controller/internal/human"
)

func main() {
	var addr, namespace string
	flag.StringVar(&addr, "addr", ":8080", "listen address for /mcp, /attention and /healthz")
	flag.StringVar(&namespace, "namespace", os.Getenv("POD_NAMESPACE"),
		"namespace Questions live in (default: POD_NAMESPACE)")
	flag.Parse()
	if namespace == "" {
		namespace = "default"
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(tinyv1.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))

	cfg, err := ctrl.GetConfig()
	if err != nil {
		log.Fatalf("kubeconfig: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Fatalf("cluster client: %v", err)
	}

	srv := &human.Server{Client: c, Namespace: namespace}
	log.Printf("tiny-human serving on %s (namespace %s): /mcp, /attention, /healthz", addr, namespace)
	s := &http.Server{
		Addr:    addr,
		Handler: srv.Handler(),
		// ask_human blocks by design; only reads are bounded.
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(s.ListenAndServe())
}
