// Copyright 2019 Istio Authors
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

package multi

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func BuildClientConfig(kubeconfig, context string) clientcmd.ClientConfig {
	if kubeconfig != "" {
		info, err := os.Stat(kubeconfig)
		if err != nil || info.Size() == 0 {
			// If the specified kubeconfig doesn't exists / empty file / any other error
			// from file stat, fall back to default
			kubeconfig = ""
		}
	}

	//Config loading rules:
	// 1. kubeconfig if it not empty string
	// 2. In cluster config if running in-cluster
	// 3. Config(s) in KUBECONFIG environment variable
	// 4. Use $HOME/.kube/config
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.DefaultClientConfig = &clientcmd.DefaultClientConfig
	loadingRules.ExplicitPath = kubeconfig
	configOverrides := &clientcmd.ConfigOverrides{
		ClusterDefaults: clientcmd.ClusterDefaults,
		CurrentContext:  context,
	}

	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
}

type args struct {
	kubeconfig string
	context    string
	clusters   []string
	namespace  string

	config clientcmd.ClientConfig
}

// list available cluster contexts
// iop multi list

// join clusters in the same flat network
// iop multi join --clusters=c0,c1,c2

func GetListCommand(args *args) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "list available clusters",
		Args:  cobra.ExactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			config, err := args.config.ConfigAccess().GetStartingConfig()
			if err != nil {
				return err
			}

			out := tabwriter.NewWriter(os.Stdout, 1, 8, 1, '\t', 0)

			fmt.Fprintf(out, "NAME\tCLUSTER\tAUTHINFO\tNAMESPACE\n")
			for name, context := range config.Contexts {
				fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", name, context.Cluster, context.AuthInfo, context.Namespace)
			}
			return out.Flush()
		},
	}

	return cmd
}

func GetJoinCommand(args *args) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "join",
		Short: "Join clusters together in a mesh",
		Args:  cobra.ExactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			config, err := args.config.ConfigAccess().GetStartingConfig()
			if err != nil {
				return err
			}

			// TODO - join to clusters first

			if len(args.clusters) != 2 {
				cmd.Printf("only two clusters supported - %v clusters specified\n", len(args.clusters))
				os.Exit(1)
			}

			csm := make(map[string]*kubernetes.Clientset, len(args.clusters))

			var notFound bool
			for _, cluster := range args.clusters {
				if _, ok := config.Contexts[cluster]; !ok {
					cmd.Printf("cluster %q configuration not found\n", cluster)
					notFound = true
					continue
				}

				rest, err := BuildClientConfig(args.kubeconfig, cluster).ClientConfig()
				if err != nil {
					cmd.Printf("could not build client for cluster %q: %v\n", cluster, err)
					notFound = true
					continue
				}

				cs, err := kubernetes.NewForConfig(rest)
				if err != nil {
					cmd.Printf("could not create clientset for cluster %q: %v\n", cluster, err)
					notFound = true
					continue
				}

				if _, err = cs.CoreV1().Namespaces().Get("istio-system", metav1.GetOptions{}); err != nil {
					// TODO - use errors.IsNotFound
					cmd.Printf("could not find istio-system namespace in cluster %q: %v\n", cluster, err)
					notFound = true
					continue
				}
				cmd.Printf("found istio-system for cluster %v\n", cluster)

				csm[cluster] = cs
			}

			if notFound {
				os.Exit(1)
			}

			// c0 := csm[args.clusters[0]]
			// c1 := csm[args.clusters[1]]

			// FLAT_NETWORK

			// - CONTROL_PLANE
			scale := func(replicas int) error {
				args := []string{
					"kubectl",
					fmt.Sprintf("--context=%v", args.clusters[1]),
					"scale",
					"deployment",
					"-n",
					"istio-system",
					"istio-citadel",
					"--replicas",
					strconv.Itoa(replicas),
				}
				if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
					return fmt.Errorf("%v: %v", err, string(out))
				}
				return nil
			}
			wait := func() error {
				args := []string{
					"kubectl",
					fmt.Sprintf("--context=%v", args.clusters[1]),
					"rollout",
					"status",
					"deployment",
					"-n",
					"istio-system",
					"istio-citadel",
				}
				if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
					return fmt.Errorf("%v: %v", err, string(out))
				}
				return nil
			}

			if err := scale(0); err != nil {
				log.Fatal(err)
			}
			if err := wait(); err != nil {
				log.Fatal(err)
			}

			// $KUBECTL_DST -n istio-system delete secret istio-ca-secret || true
			deleteSecret := func(namespace, secret string) error {
				args := strings.Split(fmt.Sprintf("kubectl --context=%v -n %v delete secret %v", args.clusters[1], namespace, secret), " ")
				fmt.Println(args)
				if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
					return fmt.Errorf("%v: %v", err, string(out))
				} else {
					fmt.Println(string(out))
				}

				return nil
			}
			if err := deleteSecret("istio-system", "istio-ca-secret"); err != nil {
				log.Fatal(err)
			}

			cargs := strings.Split(fmt.Sprintf("kubectl --context=%v get namespace -o jsonpath='{.items[*].metadata.name}'", args.clusters[1]), " ")
			out, err := exec.Command(cargs[0], cargs[1:]...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%v: %v", err, string(out))
			}

			// TODO - this should delete *all* Istio secrets
			namespaces := strings.Split(string(out), " ")
			fmt.Println("NS", namespaces)
			for _, namespace := range namespaces {
				args := strings.Split(fmt.Sprintf("kubectl --context=%v -n %v delete secret istio.default", namespace, args.clusters[1]), " ")
				exec.Command(args[0], args[1:]...).CombinedOutput()
			}

			cargs = strings.Split(fmt.Sprintf("kubectl --context=%v -n istio-system get secret istio-ca-secret -o yaml --export", args.clusters[0]), " ")
			out, err = exec.Command(cargs[0], cargs[1:]...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%v: %v", err, string(out))
			}

			t, err := ioutil.TempFile("", "")
			if err != nil {
				log.Fatal(err)
			}
			_, err = t.Write(out)
			if err != nil {
				log.Fatal(err)
			}
			t.Close()

			fmt.Println("saved to ", t.Name())
			cargs = strings.Split(fmt.Sprintf("kubectl --context=%v -n istio-system apply -f %v --validate=false", args.clusters[1], t.Name()), " ")
			out, err = exec.Command(cargs[0], cargs[1:]...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%v: %v", err, string(out))
			}

			if err := scale(1); err != nil {
				log.Fatal(err)
			}
			if err := wait(); err != nil {
				log.Fatal(err)
			}

			patch := "{\"spec\":{\"template\":{\"metadata\":{\"annotations\":{\"date\":\"`date +'%s'`\"}}}}}"

			for _, namespace := range namespaces {
				fmt.Println("NAMESPACE", namespace)
				switch namespace {
				case "kube-system", "kube-public":
				default:
					cargs := strings.Split(fmt.Sprintf("kubectl --context=%v -n %v get deployment -o=name", args.clusters[1], namespace), " ")
					out, err := exec.Command(cargs[0], cargs[1:]...).CombinedOutput()
					if err != nil {
						log.Fatalf("%v: %v", err, string(out))
					}
					for _, deployment := range strings.Split(string(out), "\n") {
						fmt.Println("DEPLOYMENT", deployment)
						cargs = strings.Split(fmt.Sprintf("kubectl --context=%v -n %v patch %v -p %v", args.clusters[1], namespace, deployment, patch), " ")
						fmt.Println(cargs)
						out, err = exec.Command(cargs[0], cargs[1:]...).CombinedOutput()
						if err != nil {
							log.Fatalf("%v: %v", err, string(out))
						}
						fmt.Println(string(out))
					}
				}
			}

			// 	for ns in `$KUBECTL get ns -o=jsonpath="{.items[*].metadata.name}"`; do
			// 	if [[ ! $ns =~ ^namespace/kube- ]]; then
			// 	  for depl in `$KUBECTL -n $ns get deployment -o=name`; do
			// 		$KUBECTL -n $ns patch $depl -p ""
			// 	  done
			// 	fi
			//   done

			//   for ns in `$KUBECTL get ns -o=jsonpath="{.items[*].metadata.name}"`; do
			// 	if [[ ! $ns =~ ^namespace/kube- ]]; then
			// 	  for depl in `$KUBECTL -n $ns get deployment -o=name`; do
			// 		$KUBECTL -n $ns rollout status $depl || true
			// 	  done
			// 	fi
			// done

			return nil
		},
	}

	return cmd
}

func GetCommand() *cobra.Command {
	var args args

	cmd := &cobra.Command{
		Use:   "multi",
		Short: "Setup a multi-cluster mesh",
		Args:  cobra.ExactArgs(0),
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			args.config = BuildClientConfig(args.kubeconfig, args.context)
			return nil
		},
	}

	cmd.AddCommand(GetListCommand(&args))
	cmd.AddCommand(GetJoinCommand(&args))

	cmd.PersistentFlags().StringVar(&args.kubeconfig, "kubeconfig", "", "kubeconfig file")
	cmd.PersistentFlags().StringVar(&args.context, "context", "", "current context")
	cmd.PersistentFlags().StringSliceVar(&args.clusters, "clusters", nil, "cluster contexts")
	cmd.PersistentFlags().StringVarP(&args.namespace, "namespace", "n", "", "namespace")

	return cmd
}
