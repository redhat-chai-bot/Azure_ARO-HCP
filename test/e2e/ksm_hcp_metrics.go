// Copyright 2026 Microsoft Corporation
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

package e2e

import (
	"context"
	"net/http"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Azure/ARO-HCP/test/util/config"
	"github.com/Azure/ARO-HCP/test/util/framework"
	"github.com/Azure/ARO-HCP/test/util/labels"
	promutil "github.com/Azure/ARO-HCP/test/util/prometheus"
)

var _ = Describe("KSM HCP Metrics", func() {
	It("kube_node_info metrics should be present in Azure Monitor for the happy-path cluster",
		labels.RequireHappyPathInfra,
		labels.Medium,
		labels.Positive,
		labels.DevelopmentOnly,
		labels.AroRpApiCompatible,
		labels.RequiresConfig,
		labels.MIContainers(0),
		func(ctx context.Context) {
			tc := framework.NewTestContext()

			serviceConfig, err := config.GetServiceConfig()
			Expect(err).NotTo(HaveOccurred(), "failed to load service config")

			regionRGStr, err := config.GetStringByPath(serviceConfig, "regionRG")
			Expect(err).NotTo(HaveOccurred(), "failed to resolve regionRG")

			hcpWorkspaceNameStr, err := config.GetStringByPath(serviceConfig, "monitoring.hcpWorkspaceName")
			Expect(err).NotTo(HaveOccurred(), "failed to resolve monitoring.hcpWorkspaceName")

			// The HCP workspace lives in the mgmt (underlay infra) subscription, not the
			// customer subscription that tc.SubscriptionID resolves to (the one the e2e
			// test's own HCP cluster is created in), so it must be looked up separately.
			mgmtSubscriptionNameStr, err := config.GetStringByPath(serviceConfig, "mgmt.subscription.key")
			Expect(err).NotTo(HaveOccurred(), "failed to resolve mgmt.subscription.key")

			cred, err := tc.AzureCredential()
			Expect(err).NotTo(HaveOccurred(), "failed to get Azure credential")

			subscriptionsClientFactory, err := tc.GetARMSubscriptionsClientFactory()
			Expect(err).NotTo(HaveOccurred(), "failed to get ARM subscriptions client factory")

			subscriptionID, err := framework.GetSubscriptionID(ctx, subscriptionsClientFactory.NewClient(), mgmtSubscriptionNameStr)
			Expect(err).NotTo(HaveOccurred(), "failed to look up mgmt subscription ID for %q", mgmtSubscriptionNameStr)

			By("Resolving HCP workspace Prometheus endpoint")
			endpoint, err := promutil.LookupPrometheusEndpoint(ctx, cred, subscriptionID, regionRGStr, hcpWorkspaceNameStr)
			Expect(err).NotTo(HaveOccurred(), "failed to look up HCP Prometheus endpoint")

			type metricCheck struct {
				query       string
				description string
			}

			checks := []metricCheck{
				{
					query:       `ingresscontroller_info{hostedcontrolplane=~".+", container="kube-state-metrics"}`,
					description: "ingresscontroller_info from kube-state-metrics",
				},
				{
					query:       `kube_node_status_condition{hostedcontrolplane=~".+", container="kube-state-metrics"}`,
					description: "kube_node_status_condition from kube-state-metrics",
				},
				{
					query:       `kube_node_info{hostedcontrolplane=~".+", container="kube-state-metrics"}`,
					description: "kube_node_info from kube-state-metrics",
				},
			}

			httpClient := &http.Client{Timeout: 30 * time.Second}

			// Track which metrics have been found so we don't re-query them.
			found := make(map[string]bool, len(checks))

			By("Polling Azure Monitor for KSM HCP metrics")
			// Azure Monitor Prometheus ingestion latency for new metric series can exceed 10 minutes.
			Eventually(func(g Gomega) {
				now := time.Now()
				// Ingestion latency (noted above) can exceed 10 minutes, so a
				// generous lookback prevents missing samples that land with an
				// older timestamp once ingestion catches up.
				start := now.Add(-35 * time.Minute)

				var missing []string
				for _, c := range checks {
					if found[c.query] {
						continue
					}

					resp, err := promutil.QueryRange(ctx, httpClient, cred, endpoint, c.query, start, now, "60s")
					g.Expect(err).NotTo(HaveOccurred(), "Prometheus query_range failed for %s", c.description)
					if err != nil {
						return
					}

					if len(resp.Data.Result) > 0 {
						found[c.query] = true
						GinkgoLogr.Info("metric found", "metric", c.description)
					} else {
						missing = append(missing, c.description)
					}
				}

				if len(missing) > 0 {
					GinkgoLogr.Info("poll status",
						"found", len(found),
						"total", len(checks),
						"missingMetrics", strings.Join(missing, ", "))
				}

				g.Expect(missing).To(BeEmpty(),
					"expected %s but got no results", strings.Join(missing, "; "))
			}).WithTimeout(25*time.Minute).WithPolling(30*time.Second).WithContext(ctx).Should(Succeed(),
				"not all KSM metrics appeared in Azure Monitor")

			for _, c := range checks {
				GinkgoWriter.Printf("  [OK] %s\n", c.description)
			}
		})
})
