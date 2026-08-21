package logging

import (
	"github.com/openshift/openshift-logging-e2e-tests/test/e2e/testdata"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	exutil "github.com/openshift/origin/test/extended/util"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

var _ = g.Describe("[sig-openshift-logging] Logging NonPreRelease", func() {
	defer g.GinkgoRecover()
	var (
		oc = exutil.NewCLIWithoutNamespace("vector-kafka")
		loggingBaseDir string
	)

	g.Context("Log Forward to Kafka", func() {
		g.BeforeEach(func() {
			loggingBaseDir = testdata.FixturePath("logging")
			CLO := SubscriptionObjects{
				OperatorName:  "cluster-logging-operator",
				Namespace:     cloNS,
				PackageName:   "cluster-logging",
				Subscription:  filepath.Join(loggingBaseDir, "subscription", "sub-template.yaml"),
				OperatorGroup: filepath.Join(loggingBaseDir, "subscription", "allnamespace-og.yaml"),
			}
			g.By("deploy CLO")
			CLO.SubscribeOperator(oc)
			oc.SetupProject()
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:ikanse-Medium-49369-Vector Forward logs to kafka topic via Mutual Chained certificates", func() {
			g.By("Create log producer")
			appProj := oc.Namespace()
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			kafkaNS := "openshift-kafka-" + getRandomString()
			defer oc.AsAdmin().WithoutNamespace().Run("delete").Args("project", kafkaNS, "--wait=false").Execute()
			err = oc.AsAdmin().WithoutNamespace().Run("create").Args("ns", kafkaNS).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			kafka := kafka{
				namespace:      kafkaNS,
				kafkasvcName:   "kafka",
				zoosvcName:     "zookeeper",
				authtype:       "plaintext-ssl",
				pipelineSecret: "kafka-vector",
				collectorType:  "vector",
				loggingNS:      appProj,
			}
			g.By("Deploy zookeeper")
			defer kafka.removeZookeeper(oc)
			kafka.deployZookeeper(oc)
			g.By("Deploy kafka")
			defer kafka.removeKafka(oc)
			kafka.deployKafka(oc)
			kafkaEndpoint := "tls://" + kafka.kafkasvcName + "." + kafka.namespace + ".svc.cluster.local:9093/clo-topic"

			g.By("Create clusterlogforwarder")
			clf := clusterlogforwarder{
				name:                   "clf-49369",
				namespace:              appProj,
				templateFile:           filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "clf-kafka-no-auth.yaml"),
				secretName:             kafka.pipelineSecret,
				waitForPodReady:        true,
				collectApplicationLogs: true,
				serviceAccountName:     "clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			clf.create(oc, "URL="+kafkaEndpoint)

			g.By("Check app logs in kafka consumer pod")
			consumerPodPodName, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("pods", "-n", kafka.namespace, "-l", "component=kafka-consumer", "-o", "name").Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			err = wait.PollUntilContextTimeout(context.Background(), 10*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
				appLogs, err := getDataFromKafkaByNamespace(oc, kafka.namespace, consumerPodPodName, appProj)
				if err != nil {
					return false, err
				}
				return len(appLogs) > 0, nil
			})
			compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("App logs are not found in %s/%s", kafka.namespace, consumerPodPodName))
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:ikanse-Medium-52420-Vector Forward logs to kafka using SASL plaintext", func() {
			g.By("Create log producer")
			appProj := oc.Namespace()
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			kafkaNS := "openshift-kafka-" + getRandomString()
			defer oc.AsAdmin().WithoutNamespace().Run("delete").Args("project", kafkaNS, "--wait=false").Execute()
			err = oc.AsAdmin().WithoutNamespace().Run("create").Args("ns", kafkaNS).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			g.By("Deploy zookeeper")
			kafka := kafka{
				namespace:      kafkaNS,
				kafkasvcName:   "kafka",
				zoosvcName:     "zookeeper",
				authtype:       "sasl-plaintext",
				pipelineSecret: "vector-kafka",
				collectorType:  "vector",
				loggingNS:      appProj,
			}
			defer kafka.removeZookeeper(oc)
			kafka.deployZookeeper(oc)
			g.By("Deploy kafka")
			defer kafka.removeKafka(oc)
			kafka.deployKafka(oc)
			kafkaEndpoint := "tcp://" + kafka.kafkasvcName + "." + kafka.namespace + ".svc.cluster.local:9092/clo-topic"

			g.By("Create clusterlogforwarder")
			clf := clusterlogforwarder{
				name:                   "clf-52420",
				namespace:              appProj,
				templateFile:           filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "clf-kafka-with-auth.yaml"),
				secretName:             kafka.pipelineSecret,
				waitForPodReady:        true,
				collectApplicationLogs: true,
				serviceAccountName:     "clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			clf.create(oc, "URL="+kafkaEndpoint)

			// Remove tls configuration from CLF as it is not required for this case
			patch := `[{"op": "remove", "path": "/spec/outputs/0/tls"}]`
			clf.update(oc, "", patch, "--type=json")
			WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)

			g.By("Check app logs in kafka consumer pod")
			consumerPodPodName, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("pods", "-n", kafka.namespace, "-l", "component=kafka-consumer", "-o", "name").Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			err = wait.PollUntilContextTimeout(context.Background(), 10*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
				appLogs, err := getDataFromKafkaByNamespace(oc, kafka.namespace, consumerPodPodName, appProj)
				if err != nil {
					return false, err
				}
				return len(appLogs) > 0, nil
			})
			compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("App logs are not found in %s/%s", kafka.namespace, consumerPodPodName))
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:anli-WRS-Critical-68312-V-ICA.02-V-ICA.03-Forward to Kafka using SSL-SASL_SCRAM auth", func() {
			amqNS := oc.Namespace()

			g.By("crete kafka instance")
			amqi := amqInstance{
				name:         "my-cluster",
				namespace:    amqNS,
				topicPrefix:  "logging-topic",
				instanceType: "kafka-sasl-cluster",
			}
			defer amqi.destroy(oc)
			amqi.deploy(oc)
			topicName := "logging-topic-52496"
			consumerPodName := amqi.createTopicAndConsumber(oc, topicName)

			g.By("create log producer")
			oc.SetupProject()
			appProj := oc.Namespace()
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err := oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("forward logs to Kafkas using ssl sasl scram-sha-512")
			clf := clusterlogforwarder{
				name:                      "clf-52496",
				namespace:                 amqNS,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "clf-kafka-sasl-ssl.yaml"),
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          false,
				collectInfrastructureLogs: false,
				serviceAccountName:        "clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			secretName := "secret-for-kafka-52420"
			oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", secretName, "-n", clf.namespace, "--from-literal=username="+amqi.user, "--from-literal=password="+amqi.password, "--from-literal=ca-bundle.crt="+amqi.routeCA).Execute()
			//To reduce the logs collected, we only collect app logs from appProj
			//Note: all sasl and tls data are from secret clf-to-amq with fix name -- user,password,ca
			clf.create(oc, "URL=tls://"+amqi.route+"/"+topicName, "SECRET_NAME="+secretName, "NAMESPACE_PATTERN="+appProj)

			g.By("verifiy the data are sent to kafka")
			//application logs
			logs, err := getDataFromKafkaConsumerPod(oc, amqi.namespace, consumerPodName)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(len(logs) > 0).Should(o.BeTrue(), "Can not find any logs from kafka consumer pods")
		})

		// author qitang@redhat.com
		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:qitang-Medium-47036-Vector Forward logs to different AMQ Kafka topics[Slow]", func() {
			nodes, err := oc.AdminKubeClient().CoreV1().Nodes().List(context.Background(), metav1.ListOptions{LabelSelector: "kubernetes.io/os=linux,kubernetes.io/arch=amd64"})
			if err != nil || len(nodes.Items) == 0 {
				g.Skip("Skip for the cluster doesn't have amd64 node")
			}
			amqNS := oc.Namespace()

			g.By("crete kafka instance")
			amqi := amqInstance{
				name:         "my-cluster",
				namespace:    amqNS,
				topicPrefix:  "topic-logging",
				instanceType: "kafka-sasl-cluster",
			}
			defer amqi.destroy(oc)
			amqi.deploy(oc)
			//topic name are fix value in clf_kafka_multi_topics.yaml
			consumerAppPodName := amqi.createTopicAndConsumber(oc, amqi.topicPrefix+"-app")
			consumerInfraPodName := amqi.createTopicAndConsumber(oc, amqi.topicPrefix+"-infra")
			consumerAuditPodName := amqi.createTopicAndConsumber(oc, amqi.topicPrefix+"-audit")

			g.By("create log producer")
			oc.SetupProject()
			appProj := oc.Namespace()
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err = oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("forward logs to Kafkas")
			clf := clusterlogforwarder{
				name:                      "clf-47036",
				namespace:                 amqNS,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "clf-kafka-multi-topics.yaml"),
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: true,
				serviceAccountName:        "clf-" + getRandomString(),
			}
			secretName := "secret-for-kafka-47036"
			oc.NotShowInfo().AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", secretName, "-n", clf.namespace, "--from-literal=username="+amqi.user, "--from-literal=password="+amqi.password, "--from-literal=ca-bundle.crt="+amqi.routeCA).Execute()
			defer clf.delete(oc)
			clf.create(oc, "BOOTSTRAP_SVC="+amqi.service, "NAMESPACE_PATTERN="+appProj, "APP_TOPIC="+amqi.topicPrefix+"-app", "INFRA_TOPIC="+amqi.topicPrefix+"-infra", "AUDIT_TOPIC="+amqi.topicPrefix+"-audit", "SECRET_NAME="+secretName)
			g.By("check data in kafka")
			//app logs
			appLogs, err := getDataFromKafkaConsumerPod(oc, amqNS, consumerAppPodName)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(len(appLogs) > 0).Should(o.BeTrue(), "Can not find any logs from topic-logging-app-consumer")
			e2e.Logf("found app logs \n")

			//infrastructure logs
			infraLogs, err := getDataFromKafkaConsumerPod(oc, amqNS, consumerInfraPodName)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(len(infraLogs) > 0).Should(o.BeTrue(), "Can not find any logs from topic-logging-infra-consumer")
			o.Expect(infraLogs[0].LogType == "infrastructure").Should(o.BeTrue(), "Can not find infra logs in consumer pod")
			e2e.Logf("found infra logs \n")

			//audit logs
			auditLogs, err := getDataFromKafkaConsumerPod(oc, amqNS, consumerAuditPodName)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(len(auditLogs) > 0).Should(o.BeTrue(), "Can not find any logs from topic-logging-audit-consumer")
			o.Expect(auditLogs[0].LogType == "audit").Should(o.BeTrue(), "Can not find audit logs in consumer pod")
			e2e.Logf("found audit logs \n")
		})

		// author qitang@redhat.com
		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:qitang-Medium-48141-Medium-85766-Vector Forward logs to different Kafka brokers and test NetworkPolicy for Kafka multi-brokers.[Slow]", func() {
			nodes, err := oc.AdminKubeClient().CoreV1().Nodes().List(context.Background(), metav1.ListOptions{LabelSelector: "kubernetes.io/os=linux,kubernetes.io/arch=amd64"})
			if err != nil || len(nodes.Items) == 0 {
				g.Skip("Skip for the cluster doesn't have amd64 node")
			}
			//create project at first , so the OLM has more time to prepare CSV in these namespaces
			amqi1NS := oc.Namespace()
			oc.SetupProject()
			amqi2NS := oc.Namespace()
			g.By("deploy AMQ kafka instance in two different namespaces")
			// to avoid collecting kafka logs, deploy kafka in project openshift-*
			// In general, we send data to brokers in kafka cluster. for historical, we use two clusters here. toDo: launch one cluster with more than one broker
			topicName := "topic-logging"
			amqi1 := amqInstance{
				name:         "my-cluster",
				namespace:    amqi1NS,
				topicPrefix:  topicName,
				instanceType: "kafka-no-auth-cluster",
			}
			amqi2 := amqInstance{
				name:         "my-cluster",
				namespace:    amqi2NS,
				topicPrefix:  topicName,
				instanceType: "kafka-no-auth-cluster",
			}

			defer amqi1.destroy(oc)
			amqi1.deploy(oc)
			amqi1ConsumerPodName := amqi1.createTopicAndConsumber(oc, topicName)
			defer amqi2.destroy(oc)
			amqi2.deploy(oc)
			amqi2ConsumerPodName := amqi2.createTopicAndConsumber(oc, topicName)

			g.By("create log producer")
			oc.SetupProject()
			appProj := oc.Namespace()
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			// avoid hitting https://issues.redhat.com/browse/LOG-3025, set replicas to 3
			err = oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile, "-p", "REPLICAS=3").Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("forward logs to Kafkas")
			clf := clusterlogforwarder{
				name:                      "clf-48141",
				namespace:                 amqi1NS,
				templateFile:              filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "clf-kafka-multi-brokers.yaml"),
				waitForPodReady:           true,
				collectApplicationLogs:    true,
				collectAuditLogs:          true,
				collectInfrastructureLogs: false,
				serviceAccountName:        "clf-" + getRandomString(),
				enableMonitoring:          true,
			}
			defer clf.delete(oc)
			brokers, _ := json.Marshal([]string{"tls://" + amqi1.service, "tls://" + amqi2.service})
			clf.create(oc, "TOPIC="+topicName, "BROKERS="+string(brokers), "NAMESPACE_PATTERN="+appProj, `COLLECTOR={"networkPolicy": {"ruleSet": "RestrictIngressEgress"}}`)
			clf.checkNetworkPolicy(oc)
			o.Expect(clf.validateNetworkPolicy(oc, "RestrictIngressEgress", []Port{{"9092", "TCP"}}, nil)).NotTo(o.HaveOccurred())

			g.By("check data in the first broker")
			amqi1logs, _ := getDataFromKafkaConsumerPod(oc, amqi1.namespace, amqi1ConsumerPodName)
			o.Expect(len(amqi1logs) > 0).Should(o.BeTrue(), "Can not fetch any logs from broker1-consumer")

			g.By("check data in the second broker")
			amqi2logs, _ := getDataFromKafkaConsumerPod(oc, amqi2.namespace, amqi2ConsumerPodName)
			o.Expect(len(amqi2logs) > 0).Should(o.BeTrue(), "Can not fetch any logs from broker2-consumer")

			g.By("check metrics exposed by collector")
			promToken := getSAToken(oc, "prometheus-k8s", "openshift-monitoring")
			checkMetric(oc, promToken, "{job=\""+clf.name+"\", namespace=\""+clf.namespace+"\"}", 3)
			checkMetric(oc, promToken, `vector_component_received_events_total{job="`+clf.name+`", namespace="`+clf.namespace+`"}`, 3)

			g.By("update network policy to AllowAllIngressEgress")
			clf.updateNetworkPolicyRuleSet(oc, "AllowAllIngressEgress")
			g.By("sleep 10 seconds for CLO to update the networkpolicy")
			time.Sleep(10 * time.Second)
			o.Expect(clf.validateNetworkPolicy(oc, "AllowAllIngressEgress", nil, nil)).NotTo(o.HaveOccurred())

			g.By("disable network policy in CLF, networkpolicy should be removed")
			clf.updateNetworkPolicyRuleSet(oc, "")
			err = resource{"networkpolicy", "collector-" + clf.name, clf.namespace}.WaitUntilResourceIsGone(oc)
			compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("networkpolicy/collector-%s in project/%s is not deleted", clf.name, clf.namespace))
		})

		// port=unknown - no data in BigQuery last 60 days
		g.It("Author:ikanse-High-61549-Collector-External Kafka output complies with the tlsSecurityProfile configuration.[Slow][Disruptive]", func() {

			compat_otp.By("Check if the cluster has amd64 nodes")
			nodes, err := oc.AdminKubeClient().CoreV1().Nodes().List(context.Background(), metav1.ListOptions{LabelSelector: "kubernetes.io/os=linux,kubernetes.io/arch=amd64"})
			if err != nil || len(nodes.Items) == 0 {
				g.Skip("Cluster doesn't have amd64 nodes, skipping the test...")
			}
			compat_otp.By("Check if the current tlsSecurityProfile is the expected one")
			if !compareExpectedTLSConfigWithCurrent(oc, `{"custom":{"ciphers":["ECDHE-ECDSA-CHACHA20-POLY1305","ECDHE-RSA-CHACHA20-POLY1305","ECDHE-RSA-AES128-GCM-SHA256","ECDHE-ECDSA-AES128-GCM-SHA256"],"minTLSVersion":"VersionTLS12"},"type":"Custom"}`) {
				g.Skip("Current tlsSecurityProfile is not the expected one, skipping the test...")
			}

			compat_otp.By("Deploy zookeeper")
			kafka := kafka{
				namespace:      loggingNS,
				kafkasvcName:   "kafka",
				zoosvcName:     "zookeeper",
				authtype:       "sasl-ssl",
				pipelineSecret: "vector-kafka",
				collectorType:  "vector",
				loggingNS:      loggingNS,
			}
			defer kafka.removeZookeeper(oc)
			kafka.deployZookeeper(oc)
			compat_otp.By("Deploy kafka")
			defer kafka.removeKafka(oc)
			kafka.deployKafka(oc)
			kafkaEndpoint := "tls://" + kafka.kafkasvcName + "." + kafka.namespace + ".svc.cluster.local:9093/clo-topic"

			compat_otp.By("Create log producer")
			appProj := oc.Namespace()
			jsonLogFile := filepath.Join(loggingBaseDir, "generatelog", "container_json_log_template.json")
			err = oc.WithoutNamespace().Run("new-app").Args("-n", appProj, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			compat_otp.By("Create clusterlogforwarder")
			clf := clusterlogforwarder{
				name:                   "clf-61549",
				namespace:              loggingNS,
				templateFile:           filepath.Join(loggingBaseDir, "observability.openshift.io_clusterlogforwarder", "clf-kafka-with-auth.yaml"),
				secretName:             kafka.pipelineSecret,
				waitForPodReady:        true,
				collectApplicationLogs: true,
				serviceAccountName:     "test-clf-" + getRandomString(),
			}
			defer clf.delete(oc)
			clf.create(oc, "URL="+kafkaEndpoint, "TLS_SECRET_NAME="+clf.secretName)

			compat_otp.By("The Kafka sink in Vector config must use the Custom tlsSecurityProfile")
			searchString := `[sinks.output_kafka_app.tls]
enabled = true
min_tls_version = "VersionTLS12"
ciphersuites = "ECDHE-ECDSA-CHACHA20-POLY1305,ECDHE-RSA-CHACHA20-POLY1305,ECDHE-RSA-AES128-GCM-SHA256,ECDHE-ECDSA-AES128-GCM-SHA256"
key_file = "/var/run/ocp-collector/secrets/vector-kafka/tls.key"
crt_file = "/var/run/ocp-collector/secrets/vector-kafka/tls.crt"
ca_file = "/var/run/ocp-collector/secrets/vector-kafka/ca-bundle.crt"`
			result, err := checkCollectorConfiguration(oc, clf.namespace, clf.name+"-config", searchString)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(result).To(o.BeTrue(), "the configuration %s is not in vector.toml", searchString)

			compat_otp.By("Check app logs in kafka consumer pod")
			consumerPodPodName, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("pods", "-n", kafka.namespace, "-l", "component=kafka-consumer", "-o", "name").Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			err = wait.PollUntilContextTimeout(context.Background(), 10*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
				appLogs, err := getDataFromKafkaByNamespace(oc, kafka.namespace, consumerPodPodName, appProj)
				if err != nil {
					return false, err
				}
				return len(appLogs) > 0, nil
			})
			compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("App logs are not found in %s/%s", kafka.namespace, consumerPodPodName))

			compat_otp.By("Set Old tlsSecurityProfile for the External Kafka output.")
			patch := `[{"op": "add", "path": "/spec/outputs/0/tls/securityProfile", "value": {"type": "Old"}}]`
			clf.update(oc, "", patch, "--type=json")
			WaitForDaemonsetPodsToBeReady(oc, clf.namespace, clf.name)

			compat_otp.By("Deploy the log generator app")
			oc.SetupProject()
			appProj1 := oc.Namespace()
			err = oc.WithoutNamespace().Run("new-app").Args("-n", appProj1, "-f", jsonLogFile).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())

			compat_otp.By("The Kafka sink in Vector config must use the Old tlsSecurityProfile")
			searchString = `[sinks.output_kafka_app.tls]
enabled = true
min_tls_version = "VersionTLS10"
ciphersuites = "TLS_AES_128_GCM_SHA256,TLS_AES_256_GCM_SHA384,TLS_CHACHA20_POLY1305_SHA256,ECDHE-ECDSA-AES128-GCM-SHA256,ECDHE-RSA-AES128-GCM-SHA256,ECDHE-ECDSA-AES256-GCM-SHA384,ECDHE-RSA-AES256-GCM-SHA384,ECDHE-ECDSA-CHACHA20-POLY1305,ECDHE-RSA-CHACHA20-POLY1305,DHE-RSA-AES128-GCM-SHA256,DHE-RSA-AES256-GCM-SHA384,DHE-RSA-CHACHA20-POLY1305,ECDHE-ECDSA-AES128-SHA256,ECDHE-RSA-AES128-SHA256,ECDHE-ECDSA-AES128-SHA,ECDHE-RSA-AES128-SHA,ECDHE-ECDSA-AES256-SHA384,ECDHE-RSA-AES256-SHA384,ECDHE-ECDSA-AES256-SHA,ECDHE-RSA-AES256-SHA,DHE-RSA-AES128-SHA256,DHE-RSA-AES256-SHA256,AES128-GCM-SHA256,AES256-GCM-SHA384,AES128-SHA256,AES256-SHA256,AES128-SHA,AES256-SHA,DES-CBC3-SHA"
key_file = "/var/run/ocp-collector/secrets/vector-kafka/tls.key"
crt_file = "/var/run/ocp-collector/secrets/vector-kafka/tls.crt"
ca_file = "/var/run/ocp-collector/secrets/vector-kafka/ca-bundle.crt"`
			result, err = checkCollectorConfiguration(oc, clf.namespace, clf.name+"-config", searchString)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(result).To(o.BeTrue(), "the configuration %s is not in vector.toml", searchString)

			compat_otp.By("Check for errors in collector pod logs.")
			e2e.Logf("Wait for a minute before the collector logs are generated.")
			time.Sleep(60 * time.Second)
			collectorLogs, err := oc.AsAdmin().WithoutNamespace().Run("logs").Args("-n", clf.namespace, "--selector=app.kubernetes.io/component=collector").Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(strings.Contains(collectorLogs, "Error trying to connect")).ShouldNot(o.BeTrue(), "Unable to connect to the external Kafka server.")

			compat_otp.By("Check app logs in kafka consumer pod")
			consumerPodPodName, err = oc.AsAdmin().WithoutNamespace().Run("get").Args("pods", "-n", kafka.namespace, "-l", "component=kafka-consumer", "-o", "name").Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			err = wait.PollUntilContextTimeout(context.Background(), 10*time.Second, 180*time.Second, true, func(context.Context) (done bool, err error) {
				appLogs, err := getDataFromKafkaByNamespace(oc, kafka.namespace, consumerPodPodName, appProj1)
				if err != nil {
					return false, err
				}
				return len(appLogs) > 0, nil
			})
			compat_otp.AssertWaitPollNoErr(err, fmt.Sprintf("App logs are not found in %s/%s", kafka.namespace, consumerPodPodName))
		})
	})
})
