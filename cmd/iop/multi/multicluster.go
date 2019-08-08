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
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/spf13/cobra"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
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

type Args struct {
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

func GetListCommand(args *Args) *cobra.Command {
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

const pilotSecretTemplate = `
apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: ${CA_DATA}
    server: ${SERVER}
  name: ${CLUSTER_NAME}
contexts:
- context:
    cluster: ${CLUSTER_NAME}
    user: ${CLUSTER_NAME}
  name: ${CLUSTER_NAME}
current-context: ${CLUSTER_NAME}
preferences: {}
users:
- name: ${CLUSTER_NAME}
  user:
    token: ${TOKEN}
`

func GetJoinCommand(args *Args) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "join",
		Short: "Join clusters together in a mesh",
		Args:  cobra.ExactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			config, err := args.config.ConfigAccess().GetStartingConfig()
			if err != nil {
				return err
			}

			if false {

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

				// c0 := csm[Args.clusters[0]]
				// c1 := csm[Args.clusters[1]]

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
				// remove existing self-signed and externally provided certs
				if err := deleteSecret("istio-system", "istio-ca-secret"); err != nil {
					log.Print(err)
				}

				cargs := strings.Split(fmt.Sprintf("kubectl --context=%v get namespace -o jsonpath={.items[*].metadata.name}", args.clusters[1]), " ")
				out, err := exec.Command(cargs[0], cargs[1:]...).CombinedOutput()
				if err != nil {
					return fmt.Errorf("%v: %v", err, string(out))
				}

				fmt.Println("NS", string(out))

				// TODO - this should delete *all* Istio secrets
				namespaces := strings.Split(string(out), " ")

				for _, namespace := range namespaces {
					args := strings.Split(fmt.Sprintf("kubectl --context=%v -n %v delete secret istio.default", namespace, args.clusters[1]), " ")
					exec.Command(args[0], args[1:]...).CombinedOutput()
				}

				fmt.Println("copy secrets to joined cluster")
				// TODO source cluster may have self-signed or plugged cert. We need to copy one or the other (but not both) to joined cluster.
				for _, secret := range []string{"istio-ca-secret", "cacerts"} {
					cargs = strings.Split(fmt.Sprintf("kubectl --context=%v -n istio-system get secret %v -o yaml --export", args.clusters[0], secret), " ")
					out, err = exec.Command(cargs[0], cargs[1:]...).CombinedOutput()
					if err != nil {
						log.Printf("%v: %v\n", err, string(out))
						continue
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
				}

				if err := scale(1); err != nil {
					log.Fatal(err)
				}
				if err := wait(); err != nil {
					log.Fatal(err)
				}

				patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"date":"%v"}}}}}`, time.Now().UTC().Format(time.RFC3339))

				for _, namespace := range namespaces {
					switch namespace {
					case "kube-system", "kube-public":
					default:
						cargs := strings.Split(fmt.Sprintf("kubectl --context=%v -n %v get deployment -o=name", args.clusters[1], namespace), " ")
						out, err := exec.Command(cargs[0], cargs[1:]...).CombinedOutput()
						if err != nil {
							log.Fatalf("%v: %v", err, string(out))
						}
						for _, deployment := range strings.Split(string(out), "\n") {
							if deployment == "" {
								continue
							}
							cargs = strings.Split(fmt.Sprintf("kubectl --context=%v -n %v patch %v -p %s", args.clusters[1], namespace, deployment, patch), " ")
							out, err = exec.Command(cargs[0], cargs[1:]...).CombinedOutput()
							if err != nil {
								log.Fatalf("%v: %v", err, string(out))
							}
						}
					}
				}

				for _, namespace := range namespaces {
					switch namespace {
					case "kube-system", "kube-public":
					default:
						cargs := strings.Split(fmt.Sprintf("kubectl --context=%v -n %v get deployment -o=name", args.clusters[1], namespace), " ")
						out, err := exec.Command(cargs[0], cargs[1:]...).CombinedOutput()
						if err != nil {
							log.Fatalf("%v: %v", err, string(out))
						}
						for _, deployment := range strings.Split(string(out), "\n") {
							if deployment == "" {
								continue
							}
							cargs = strings.Split(fmt.Sprintf("kubectl --context=%v -n %v rollout status %v", args.clusters[1], namespace, deployment), " ")
							out, err = exec.Command(cargs[0], cargs[1:]...).CombinedOutput()
							if err != nil {
								log.Fatalf("%v: %v", err, string(out))
							}
							fmt.Println(string(out))
						}
					}
				}
			}
			// create k8s secret with c0 pilot SA kubeconfig, label, and copy to c1
			// create k8s secret with c1 pilot SA kubeconfig, label, and copy to c0

			// TODO - multiple kubeconfig context may point to the same cluster.
			for _, dst := range args.clusters {
				dstRest, err := BuildClientConfig(args.kubeconfig, dst).ClientConfig()
				if err != nil {
					log.Fatal(err)
				}

				dstKube, err := kubernetes.NewForConfig(dstRest)
				if err != nil {
					log.Fatal(err)
				}

				for _, src := range args.clusters {
					// skip self
					if src == dst {
						continue
					}
					fmt.Printf("joining %v to %v\n", src, dst)

					// local CLUSTER_NAME=$($KUBECTL_SLAVE config view -o jsonpath="{.contexts[?(@.name == \"${KUBECONTEXT_SLAVE}\")].context.cluster}")
					clusterName := config.Contexts[dst].Cluster

					// local SERVER=$($KUBECTL_SLAVE config view -o jsonpath="{.clusters[?(@.name == \"${CLUSTER_NAME}\")].cluster.server}")
					server := config.Clusters[clusterName].Server

					// local NAMESPACE=istio-system
					namespace := "istio-system"

					// local SERVICE_ACCOUNT=istio-pilot-service-account
					serviceAccountName := "istio-pilot-service-account"

					srcRest, err := BuildClientConfig(args.kubeconfig, src).ClientConfig()
					if err != nil {
						log.Fatal(err)
					}

					srcKube, err := kubernetes.NewForConfig(srcRest)
					if err != nil {
						log.Fatal(err)
					}

					// local SECRET_NAME=$($KUBECTL_SLAVE get sa ${SERVICE_ACCOUNT} -n ${NAMESPACE} -o jsonpath="{.secrets[].name}")
					serviceAccount, err := srcKube.CoreV1().ServiceAccounts(namespace).Get(serviceAccountName, metav1.GetOptions{})
					if err != nil {
						log.Fatal(err)
					}
					if len(serviceAccount.Secrets) != 1 {
						log.Fatal(err)
					}
					secretName := serviceAccount.Secrets[0].Name

					// local CA_DATA=$($KUBECTL_SLAVE get secret ${SECRET_NAME} -n ${NAMESPACE} -o jsonpath="{.data['ca\.crt']}")
					pilotSecret, err := srcKube.CoreV1().Secrets(namespace).Get(secretName, metav1.GetOptions{})
					if err != nil {
						log.Fatal(err)
					}
					caData, ok := pilotSecret.Data["ca.crt"]
					if !ok {
						log.Fatalf("%v is missing ca.crt", secretName)
					}

					// local TOKEN=$($KUBECTL_SLAVE get secret ${SECRET_NAME} -n ${NAMESPACE} -o jsonpath="{.data['token']}" | base64 --decode)
					token, ok := pilotSecret.Data["token"]
					if !ok {
						log.Fatalf("%v is missing token", secretName)
					}

					sc := api.NewConfig()
					sc.Kind = "Config"
					sc.APIVersion = "v1"
					sc.Clusters[clusterName] = &api.Cluster{
						CertificateAuthorityData: caData,
						Server:                   server,
					}
					sc.Contexts[clusterName] = &api.Context{
						Cluster:  clusterName,
						AuthInfo: clusterName,
					}
					sc.CurrentContext = clusterName
					sc.AuthInfos[clusterName] = &api.AuthInfo{
						Token: string(token),
					}

					srcSecret := &v1.Secret{
						ObjectMeta: metav1.ObjectMeta{
							Name:      fmt.Sprintf("istio-mc-%v", src),
							Namespace: namespace,
							Labels: map[string]string{
								"istio/multiCluster": "true",
							},
						},
					}

					if result, err := dstKube.CoreV1().Secrets(namespace).Create(srcSecret); errors.IsAlreadyExists(err) {
						fmt.Println("secret exists:", result)

						patch, err := json.Marshal(srcSecret)
						if err != nil {
							log.Fatal(err)
						}

						res, err := dstKube.CoreV1().Secrets(namespace).Patch(srcSecret.Name, types.StrategicMergePatchType, patch)
						fmt.Println("PATCH: err: ", err)
						fmt.Println("PATCH: result: ", res)
					}
				}
			}

			return nil
		},
	}

	return cmd
}

func GetCommand() *cobra.Command {
	var args Args

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
