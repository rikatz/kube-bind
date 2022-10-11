/*
Copyright 2022 The Kube Bind Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apiextensionsinformers "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	genericapiserver "k8s.io/apiserver/pkg/server"
	kubeinformers "k8s.io/client-go/informers"
	kubernetesclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	logsv1 "k8s.io/component-base/logs/api/v1"
	_ "k8s.io/component-base/logs/json/register"
	"k8s.io/component-base/version"
	"k8s.io/klog/v2"

	konnectoroptions "github.com/kube-bind/kube-bind/cmd/konnector/options"
	"github.com/kube-bind/kube-bind/deploy/crd"
	kubebindv1alpha1 "github.com/kube-bind/kube-bind/pkg/apis/kubebind/v1alpha1"
	bindclient "github.com/kube-bind/kube-bind/pkg/client/clientset/versioned"
	bindinformers "github.com/kube-bind/kube-bind/pkg/client/informers/externalversions"
	"github.com/kube-bind/kube-bind/pkg/konnector"
)

func New() *cobra.Command {
	options := konnectoroptions.NewOptions()
	cmd := &cobra.Command{
		Use:   "konnector",
		Short: "Connect remote API services to local APIs",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := logsv1.ValidateAndApply(options.Logs, nil); err != nil {
				return err
			}
			if err := options.Complete(); err != nil {
				return err
			}

			if err := options.Validate(); err != nil {
				return err
			}

			ctx := genericapiserver.SetupSignalContext()

			ver := version.Get().GitVersion
			if i := strings.Index(ver, "bind-"); i != -1 {
				ver = ver[i+5:] // example: v1.25.2+kubectl-bind-v0.0.7-52-g8fee0baeaff3aa
			}
			logger := klog.FromContext(ctx)
			logger.Info("Starting konnector", "version", ver)

			// create clients
			cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(&clientcmd.ClientConfigLoadingRules{
				ExplicitPath: options.KubeConfigPath,
			}, nil).ClientConfig()
			if err != nil {
				return err
			}
			bindClient, err := bindclient.NewForConfig(cfg)
			if err != nil {
				return err
			}
			kubeClient, err := kubernetesclient.NewForConfig(cfg)
			if err != nil {
				return err
			}
			apiextensionsClient, err := apiextensionsclient.NewForConfig(cfg)
			if err != nil {
				return err
			}

			// install/upgrade CRDs
			if err := crd.Create(ctx,
				apiextensionsClient.ApiextensionsV1().CustomResourceDefinitions(),
				metav1.GroupResource{Group: kubebindv1alpha1.GroupName, Resource: "servicebindings"},
			); err != nil {
				return err
			}

			// construct informer factories
			kubeInformers := kubeinformers.NewSharedInformerFactory(kubeClient, time.Minute*30)
			bindInformers := bindinformers.NewSharedInformerFactory(bindClient, time.Minute*30)
			apiextensionsInformers := apiextensionsinformers.NewSharedInformerFactory(apiextensionsClient, time.Minute*30)

			// construct controllers
			k, err := konnector.New(
				cfg,
				bindInformers.KubeBind().V1alpha1().ServiceBindings(),
				kubeInformers.Core().V1().Secrets(), // TODO(sttts): watch individual secrets for security and memory consumption
				kubeInformers.Core().V1().Namespaces(),
				apiextensionsInformers.Apiextensions().V1().CustomResourceDefinitions(),
			)
			if err != nil {
				return err
			}

			logger.Info("trying to acquire the lock")
			lock := NewLock(kubeClient, options.LeaseLockNamespace, options.LeaseLockName, options.LeaseLockIdentity)
			runLeaderElection(ctx, lock, options.LeaseLockIdentity, func(ctx context.Context) {
				// start informer factories
				logger.Info("starting informers")
				kubeInformers.Start(ctx.Done())
				bindInformers.Start(ctx.Done())
				apiextensionsInformers.Start(ctx.Done())
				kubeSynced := kubeInformers.WaitForCacheSync(ctx.Done())
				kubeBindSynced := bindInformers.WaitForCacheSync(ctx.Done())
				apiextensionsSynced := apiextensionsInformers.WaitForCacheSync(ctx.Done())

				logger.Info("local informers are synced",
					"kubeSynced", fmt.Sprintf("%v", kubeSynced),
					"kubeBindSynced", fmt.Sprintf("%v", kubeBindSynced),
					"apiextensionsSynced", fmt.Sprintf("%v", apiextensionsSynced),
				)

				logger.Info("starting konnector controller")
				k.Start(ctx, 2)
			})

			return nil
		},
	}

	options.AddFlags(cmd.Flags())

	if v := version.Get().String(); len(v) == 0 {
		cmd.Version = "<unknown>"
	} else {
		cmd.Version = v
	}

	return cmd
}
