/*
Copyright (C) 2018 Synopsys, Inc.

Licensed to the Apache Software Foundation (ASF) under one
or more contributor license agreements. See the NOTICE file
distributed with this work for additional information
regarding copyright ownership. The ASF licenses this file
to you under the Apache License, Version 2.0 (the
"License"); you may not use this file except in compliance
with the License. You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing,
software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
KIND, either express or implied. See the License for the
specific language governing permissions and limitations
under the License.
*/

package blackduck

import (
	"fmt"
	"math"
	"strings"
	"time"

	horizonapi "github.com/blackducksoftware/horizon/pkg/api"
	horizon "github.com/blackducksoftware/horizon/pkg/deployer"
	"github.com/blackducksoftware/synopsys-operator/pkg/api/blackduck/v1"
	blackduckclientset "github.com/blackducksoftware/synopsys-operator/pkg/blackduck/client/clientset/versioned"
	"github.com/blackducksoftware/synopsys-operator/pkg/blackduck/containers"
	hubutils "github.com/blackducksoftware/synopsys-operator/pkg/blackduck/util"
	"github.com/blackducksoftware/synopsys-operator/pkg/protoform"
	"github.com/blackducksoftware/synopsys-operator/pkg/util"
	routeclient "github.com/openshift/client-go/route/clientset/versioned/typed/route/v1"
	securityclient "github.com/openshift/client-go/security/clientset/versioned/typed/security/v1"
	"github.com/sirupsen/logrus"
	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Creater will store the configuration to create the Blackduck
type Creater struct {
	Config           *protoform.Config
	KubeConfig       *rest.Config
	KubeClient       *kubernetes.Clientset
	BlackduckClient  *blackduckclientset.Clientset
	osSecurityClient *securityclient.SecurityV1Client
	routeClient      *routeclient.RouteV1Client
}

// NewCreater will instantiate the Creater
func NewCreater(config *protoform.Config, kubeConfig *rest.Config, kubeClient *kubernetes.Clientset, hubClient *blackduckclientset.Clientset,
	osSecurityClient *securityclient.SecurityV1Client, routeClient *routeclient.RouteV1Client) *Creater {
	return &Creater{Config: config, KubeConfig: kubeConfig, KubeClient: kubeClient, BlackduckClient: hubClient, osSecurityClient: osSecurityClient, routeClient: routeClient}
}

// DeleteHub will delete the Black Duck Blackduck
func (hc *Creater) DeleteHub(namespace string) error {

	logrus.Infof("Deleting hub: %s", namespace)

	var err error
	// Verify whether the namespace exist
	_, err = util.GetNamespace(hc.KubeClient, namespace)
	if err != nil {
		log.Errorf("unable to find the namespace %+v because %+v", namespace, err)
	} else {
		// Delete a namespace
		err = util.DeleteNamespace(hc.KubeClient, namespace)
		if err != nil {
			log.Errorf("unable to delete the namespace %+v because %+v", namespace, err)
		}

		for {
			// Verify whether the namespace deleted
			ns, err := util.GetNamespace(hc.KubeClient, namespace)
			log.Infof("namespace: %v, status: %v", namespace, ns.Status)
			time.Sleep(10 * time.Second)
			if err != nil {
				log.Infof("deleted the namespace %+v", namespace)
				break
			}
		}
	}

	// Delete a persistent volume
	err = util.DeletePersistentVolume(hc.KubeClient, namespace)
	if err != nil {
		log.Errorf("unable to delete the pv for %+v", namespace)
	}

	// Delete a Cluster Role Binding
	err = util.DeleteClusterRoleBinding(hc.KubeClient, namespace)
	if err != nil {
		log.Errorf("unable to delete the cluster role binding for %+v", namespace)
	}
	return nil
}

// CreateHub will create the Black Duck Blackduck
func (hc *Creater) CreateHub(createHub *v1.BlackduckSpec) (string, map[string]string, bool, error) {
	log.Debugf("create Hub details for %s: %+v", createHub.Namespace, createHub)

	// Create a horizon deployer for each hub
	deployer, err := horizon.NewDeployer(hc.KubeConfig)
	if err != nil {
		return "", nil, true, fmt.Errorf("unable to create the horizon deployer because %+v", err)
	}

	// Get Containers Flavor
	hubContainerFlavor := containers.GetContainersFlavor(createHub.Size)
	log.Debugf("Hub Container Flavor: %+v", hubContainerFlavor)

	if hubContainerFlavor == nil {
		return "", nil, true, fmt.Errorf("invalid flavor type, Expected: Small, Medium, Large (or) X-Large, Actual: %s", createHub.Size)
	}

	log.Debugf("before init: %+v", &createHub)
	// Create the config-maps, secrets and postgres container
	err = hc.init(deployer, createHub, hubContainerFlavor)
	if err != nil {
		return "", nil, true, err
	}

	// Deploy config-maps, secrets and postgres container
	err = deployer.Run()
	if err != nil {
		log.Errorf("init deployments failed for %s because %+v", createHub.Namespace, err)
	}
	// time.Sleep(20 * time.Second)

	err = hc.Start(createHub)
	if err != nil {
		return "", nil, true, err
	}

	// Expose Hub
	deployer, err = horizon.NewDeployer(hc.KubeConfig)
	if err != nil {
		return "", nil, true, fmt.Errorf("unable to create the horizon deployer because %+v", err)
	}

	hc.AddExposeServices(deployer, createHub)

	err = deployer.Run()
	if err != nil {
		return "", nil, true, err
	}

	// Validate all pods are in running state
	err = util.ValidatePodsAreRunningInNamespace(hc.KubeClient, createHub.Namespace)
	if err != nil {
		return "", nil, true, err
	}

	// Retrieve the PVC volume name
	pvcVolumeNames := map[string]string{}
	if createHub.PersistentStorage {
		for _, v := range createHub.PVC {
			pvName, err := hc.getPVCVolumeName(createHub.Namespace, v.Name)
			if err != nil {
				return "", nil, false, err
			}
			pvcVolumeNames[v.Name] = pvName
		}
	}

	// OpenShift routes
	ipAddress := ""
	if hc.routeClient != nil {
		route, err := util.CreateOpenShiftRoutes(hc.routeClient, createHub.Namespace, createHub.Namespace, "Service", "webserver")
		if err != nil {
			return "", pvcVolumeNames, false, err
		}
		log.Debugf("openshift route host: %s", route.Spec.Host)
		ipAddress = route.Spec.Host
	}

	time.Sleep(1 * time.Minute)

	if strings.EqualFold(ipAddress, "") {
		ipAddress, err = hc.getLoadBalancerIPAddress(createHub.Namespace, "webserver-lb")
		if err != nil {
			ipAddress, err = hc.getNodePortIPAddress(createHub.Namespace, "webserver-np")
			if err != nil {
				return "", pvcVolumeNames, false, err
			}
		}
	}
	log.Infof("hub Ip address: %s", ipAddress)

	return ipAddress, pvcVolumeNames, false, nil
}

// Start the instance
func (hc *Creater) Start(createHub *v1.BlackduckSpec) error {
	// Create CM, secrets
	deployer, err := hc.getHubConfigDeployer(createHub)
	if err != nil {
		return err
	}
	err = deployer.Run()
	if err != nil {
		return err
	}

	// Start postgres if needed
	if createHub.ExternalPostgres == nil {
		pg, err := hc.getPostgresDeployer(createHub)
		if err != nil {
			return err
		}

		// Start postgres
		err = pg.Run()
		if err != nil {
			return err
		}

		// Initialize the DB if we don't use persistent storage or that it starts for the first time
		if !createHub.PersistentStorage || (createHub.PersistentStorage && strings.EqualFold(createHub.State, "pending")) {
			err = hc.initPostgres(createHub)
			if err != nil {
				return err
			}
		}
	}

	// Start Hub
	deployer, err = hc.getHubDeployer(createHub)
	if err != nil {
		return err
	}
	return deployer.Run()
}

// Stop the instance
func (hc *Creater) Stop(createHub *v1.BlackduckSpec) error {
	// Stop Hub
	deployer, err := hc.getHubDeployer(createHub)
	if err != nil {
		return err
	}

	err = deployer.Undeploy()
	if err != nil {
		return err
	}

	// Stop postgres if we don't use an external db
	if createHub.ExternalPostgres == nil {
		pg, err := hc.getPostgresDeployer(createHub)
		if err != nil {
			return err
		}

		err = pg.Undeploy()
		if err != nil {
			return err
		}
	}

	// Delete the config
	deployer, err = hc.getHubConfigDeployer(createHub)
	if err != nil {
		return err
	}
	err = deployer.Undeploy()
	if err != nil {
		return err
	}

	return err
}

func (hc *Creater) initPostgres(createHub *v1.BlackduckSpec) error {

	var adminPassword, userPassword, postgresPassword string
	var err error

	for dbInitTry := 0; dbInitTry < math.MaxInt32; dbInitTry++ {
		// get the secret from the default operator namespace, then copy it into the hub namespace.
		adminPassword, userPassword, postgresPassword, err = hubutils.GetDefaultPasswords(hc.KubeClient, hc.Config.Namespace)
		if err == nil {
			break
		} else {
			log.Infof("[%s] wasn't able to init database, sleeping 5 seconds.  try = %v", createHub.Namespace, dbInitTry)
			time.Sleep(5 * time.Second)
		}
	}

	// Validate postgres pod is cloned/backed up
	err = util.WaitForServiceEndpointReady(hc.KubeClient, createHub.Namespace, "postgres")
	if err != nil {
		return err
	}

	if len(createHub.DbPrototype) == 0 {
		err := InitDatabase(createHub, adminPassword, userPassword, postgresPassword)
		if err != nil {
			log.Errorf("%v: error: %+v", createHub.Namespace, err)
			return fmt.Errorf("%v: error: %+v", createHub.Namespace, err)
		}
	} else {
		_, fromPw, err := hubutils.GetHubDBPassword(hc.KubeClient, createHub.DbPrototype)
		if err != nil {
			return err
		}
		err = hubutils.CloneJob(hc.KubeClient, hc.Config.Namespace, createHub.DbPrototype, createHub.Namespace, fromPw)
		if err != nil {
			return err
		}
	}
	return nil
}

func (hc *Creater) getPostgresDeployer(createHub *v1.BlackduckSpec) (*horizon.Deployer, error) {
	// Create a horizon deployer for Postgres
	deployer, err := horizon.NewDeployer(hc.KubeConfig)
	if err != nil {
		return nil, fmt.Errorf("unable to create the horizon deployer because %+v", err)
	}

	// Get Containers Flavor
	hubContainerFlavor := containers.GetContainersFlavor(createHub.Size)
	log.Debugf("Hub Container Flavor: %+v", hubContainerFlavor)

	if hubContainerFlavor == nil {
		return nil, fmt.Errorf("invalid flavor type, Expected: Small, Medium, Large (or) X-Large, Actual: %s", createHub.Size)
	}

	containerCreater := containers.NewCreater(hc.Config, createHub, hubContainerFlavor, nil, nil, nil, nil)
	deployer.AddReplicationController(containerCreater.GetPostgresDeployment())
	deployer.AddService(containerCreater.GetPostgresService())

	return deployer, nil
}

func (hc *Creater) getHubConfigDeployer(createHub *v1.BlackduckSpec) (*horizon.Deployer, error) {
	log.Debugf("create Hub details for %s: %+v", createHub.Namespace, createHub)

	// Create a horizon deployer for each hub
	deployer, err := horizon.NewDeployer(hc.KubeConfig)
	if err != nil {
		return nil, fmt.Errorf("unable to create the horizon deployer because %+v", err)
	}

	adminPassword, userPassword, _, err := hubutils.GetDefaultPasswords(hc.KubeClient, hc.Config.Namespace)
	// Create a secret
	secrets := hc.createHubSecrets(createHub, adminPassword, userPassword)
	for _, secret := range secrets {
		deployer.AddSecret(secret)
	}

	// Create ConfigMaps
	hubContainerFlavor := containers.GetContainersFlavor(createHub.Size)
	configMaps := hc.createHubConfig(createHub, hubContainerFlavor)

	for _, configMap := range configMaps {
		deployer.AddConfigMap(configMap)
	}

	return deployer, nil
}

func (hc *Creater) getHubDeployer(createHub *v1.BlackduckSpec) (*horizon.Deployer, error) {
	log.Debugf("create Hub details for %s: %+v", createHub.Namespace, createHub)

	// Create a horizon deployer for each hub
	deployer, err := horizon.NewDeployer(hc.KubeConfig)
	if err != nil {
		return nil, fmt.Errorf("unable to create the horizon deployer because %+v", err)
	}

	// Get Containers Flavor
	hubContainerFlavor := containers.GetContainersFlavor(createHub.Size)
	log.Debugf("Hub Container Flavor: %+v", hubContainerFlavor)

	if hubContainerFlavor == nil {
		return nil, fmt.Errorf("invalid flavor type, Expected: Small, Medium, Large (or) X-Large, Actual: %s", createHub.Size)
	}

	// All ConfigMap environment variables
	allConfigEnv := []*horizonapi.EnvConfig{
		{Type: horizonapi.EnvFromConfigMap, FromName: "hub-config"},
		{Type: horizonapi.EnvFromConfigMap, FromName: "hub-db-config"},
		{Type: horizonapi.EnvFromConfigMap, FromName: "hub-db-config-granular"},
	}

	err = hc.addAnyUIDToServiceAccount(createHub)
	if err != nil {
		log.Error(err)
	}

	// Create all hub deployments
	deployer, _ = horizon.NewDeployer(hc.KubeConfig)
	hc.AddToDeployer(deployer, createHub, hubContainerFlavor, allConfigEnv)

	log.Debugf("%+v", deployer)

	return deployer, nil
}

func (hc *Creater) getPVCVolumeName(namespace string, name string) (string, error) {
	for i := 0; i < 60; i++ {
		time.Sleep(10 * time.Second)
		pvc, err := util.GetPVC(hc.KubeClient, namespace, name)
		if err != nil {
			return "", fmt.Errorf("unable to get pvc in %s namespace because %s", namespace, err.Error())
		}

		log.Debugf("pvc: %v", pvc)

		if strings.EqualFold(pvc.Spec.VolumeName, "") {
			continue
		} else {
			return pvc.Spec.VolumeName, nil
		}
	}
	return "", fmt.Errorf("timeout: unable to get pvc %s in %s namespace", namespace, namespace)
}

func (hc *Creater) getLoadBalancerIPAddress(namespace string, serviceName string) (string, error) {
	for i := 0; i < 10; i++ {
		time.Sleep(10 * time.Second)
		service, err := util.GetService(hc.KubeClient, namespace, serviceName)
		if err != nil {
			return "", fmt.Errorf("unable to get service %s in %s namespace because %s", serviceName, namespace, err.Error())
		}

		log.Debugf("[%s] service: %v", serviceName, service.Status.LoadBalancer.Ingress)

		if len(service.Status.LoadBalancer.Ingress) > 0 {
			ipAddress := service.Status.LoadBalancer.Ingress[0].IP
			return ipAddress, nil
		}
	}
	return "", fmt.Errorf("timeout: unable to get ip address for the service %s in %s namespace", serviceName, namespace)
}

func (hc *Creater) getNodePortIPAddress(namespace string, serviceName string) (string, error) {
	for i := 0; i < 10; i++ {
		time.Sleep(10 * time.Second)
		service, err := util.GetService(hc.KubeClient, namespace, serviceName)
		if err != nil {
			return "", fmt.Errorf("unable to get service %s in %s namespace because %s", serviceName, namespace, err.Error())
		}

		log.Debugf("[%s] service: %v", serviceName, service.Spec.ClusterIP)

		if !strings.EqualFold(service.Spec.ClusterIP, "") {
			ipAddress := service.Spec.ClusterIP
			return ipAddress, nil
		}
	}
	return "", fmt.Errorf("timeout: unable to get ip address for the service %s in %s namespace", serviceName, namespace)
}
