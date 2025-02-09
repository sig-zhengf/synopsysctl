/*
Copyright (C) 2019 Synopsys, Inc.

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

package synopsysctl

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/blackducksoftware/synopsysctl/pkg/alert"
	alertctl "github.com/blackducksoftware/synopsysctl/pkg/alert"
	"github.com/blackducksoftware/synopsysctl/pkg/bdba"
	"github.com/blackducksoftware/synopsysctl/pkg/blackduck"
	"github.com/blackducksoftware/synopsysctl/pkg/globals"
	"github.com/blackducksoftware/synopsysctl/pkg/opssight"
	"github.com/blackducksoftware/synopsysctl/pkg/util"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Create Command CRSpecBuilderFromCobraFlagsInterface
var createAlertCobraHelper alert.HelmValuesFromCobraFlags
var createBlackDuckCobraHelper blackduck.HelmValuesFromCobraFlags
var createOpsSightCobraHelper opssight.HelmValuesFromCobraFlags
var createBDBACobraHelper bdba.HelmValuesFromCobraFlags

// Default Base Specs for Create
var baseAlertSpec string
var baseBlackDuckSpec string
var baseOpsSightSpec string

var namespace string

var alertNativePVC bool

// createCmd creates a Synopsys resource in the cluster
var createCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a Synopsys resource in your cluster",
	RunE: func(cmd *cobra.Command, args []string) error {
		return fmt.Errorf("must specify a sub-command")
	},
}

/*
Create Alert Commands
*/

// createCmd creates an Alert instance
var createAlertCmd = &cobra.Command{
	Use:           "alert NAME -n NAMESPACE",
	Example:       "synopsysctl create alert <name> -n <namespace>",
	Short:         "Create an Alert instance",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args: func(cmd *cobra.Command, args []string) error {
		// Check the Number of Arguments
		if len(args) != 1 {
			cmd.Help()
			return fmt.Errorf("this command takes 1 argument, but got %+v", args)
		}
		return nil
	},
	PreRunE: func(cmd *cobra.Command, args []string) error {
		// Set the Global Alert Version
		if cmd.Flags().Lookup("version").Changed {
			globals.AlertVersion = cmd.Flags().Lookup("version").Value.String()
		}

		// Check the flags
		err := createAlertCobraHelper.MarkRequiredFlags(cmd.Flags(), globals.AlertVersion)
		if err != nil {
			return err
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		alertName := args[0]
		helmReleaseName := fmt.Sprintf("%s%s", alertName, globals.AlertPostSuffix)

		// Get the flags to set Helm values
		helmValuesMap, err := createAlertCobraHelper.GenerateHelmFlagsFromCobraFlags(cmd.Flags())
		if err != nil {
			return err
		}

		// Update the Helm Chart Location
		newChartVersion := "" // pass empty to UpdateHelmChartLocation if the default version should be used
		if cmd.Flags().Lookup("version").Changed {
			newChartVersion = cmd.Flags().Lookup("version").Value.String()
		}
		err = UpdateHelmChartLocation(cmd.Flags(), globals.AlertChartName, newChartVersion, &globals.AlertChartRepository)
		if err != nil {
			return fmt.Errorf("failed to set the app resources location due to %+v", err)
		}

		ok, err := util.IsNotDefaultVersionGreaterThanOrEqualTo(globals.AlertVersion, 5, 3, 1)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("creation of Alert instance is only suported for version 5.3.1 and above")
		}

		// Ensure helmValuesMap has the version set
		util.SetHelmValueInMap(helmValuesMap, []string{"version"}, globals.AlertVersion)

		// Check Dry Run before deploying any resources
		err = util.CreateWithHelm3(helmReleaseName, namespace, globals.AlertChartRepository, helmValuesMap, kubeConfigPath, true)
		if err != nil {
			cleanErrorMsg := cleanAlertHelmError(err.Error(), helmReleaseName, alertName)
			return fmt.Errorf("failed to create Alert resources: %+v", cleanErrorMsg)
		}

		// Create secrets for Alert
		certificateFlag := cmd.Flag("certificate-file-path")
		certificateKeyFlag := cmd.Flag("certificate-key-file-path")
		if certificateFlag.Changed && certificateKeyFlag.Changed {
			certificateData, err := util.ReadFileData(certificateFlag.Value.String())
			if err != nil {
				log.Fatalf("failed to read certificate file: %+v", err)
			}

			certificateKeyData, err := util.ReadFileData(certificateKeyFlag.Value.String())
			if err != nil {
				log.Fatalf("failed to read certificate file: %+v", err)
			}
			customCertificateSecretName := "alert-custom-certificate"
			customCertificateSecret := alert.GetAlertCustomCertificateSecret(namespace, customCertificateSecretName, certificateData, certificateKeyData)
			util.SetHelmValueInMap(helmValuesMap, []string{"webserverCustomCertificatesSecretName"}, customCertificateSecretName)
			if _, err := kubeClient.CoreV1().Secrets(namespace).Create(context.TODO(), &customCertificateSecret, metav1.CreateOptions{}); err != nil && !k8serrors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create certifacte secret: %+v", err)
			}
		}

		javaKeystoreFlag := cmd.Flag("java-keystore-file-path")
		if javaKeystoreFlag.Changed {
			javaKeystoreData, err := util.ReadFileData(javaKeystoreFlag.Value.String())
			if err != nil {
				log.Fatalf("failed to read Java Keystore file: %+v", err)
			}
			javaKeystoreSecretName := "alert-java-keystore"
			javaKeystoreSecret := alert.GetAlertJavaKeystoreSecret(namespace, javaKeystoreSecretName, javaKeystoreData)
			util.SetHelmValueInMap(helmValuesMap, []string{"javaKeystoreSecretName"}, javaKeystoreSecretName)
			if _, err := kubeClient.CoreV1().Secrets(namespace).Create(context.TODO(), &javaKeystoreSecret, metav1.CreateOptions{}); err != nil && !k8serrors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create javakeystore secret: %+v", err)
			}
		}

		// Expose Services for Alert
		err = alert.CRUDServiceOrRoute(restconfig, kubeClient, namespace, alertName, helmValuesMap["exposeui"], helmValuesMap["exposedServiceType"], cmd.Flags().Lookup("expose-ui").Changed)
		if err != nil {
			return err
		}

		// Deploy Alert Resources
		err = util.CreateWithHelm3(helmReleaseName, namespace, globals.AlertChartRepository, helmValuesMap, kubeConfigPath, false)
		if err != nil {
			cleanErrorMsg := cleanAlertHelmError(err.Error(), helmReleaseName, alertName)
			return fmt.Errorf("failed to create Alert resources: %+v", cleanErrorMsg)
		}

		log.Infof("Alert has been successfully Created!")
		return nil
	},
}

// createAlertNativeCmd prints the Kubernetes resources for creating an Alert instance
var createAlertNativeCmd = &cobra.Command{
	Use:           "native NAME -n NAMESPACE",
	Example:       "synopsysctl create alert native <name> -n <namespace>",
	Short:         "Print the Kubernetes resources for creating an Alert instance",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args: func(cmd *cobra.Command, args []string) error {
		// Check the Number of Arguments
		if len(args) != 1 {
			cmd.Help()
			return fmt.Errorf("this command takes 1 argument, but got %+v", args)
		}
		return nil
	},
	PreRunE: func(cmd *cobra.Command, args []string) error {
		// Set the Global Alert Version
		if cmd.Flags().Lookup("version").Changed {
			globals.AlertVersion = cmd.Flags().Lookup("version").Value.String()
		}
		// Check the flags
		err := createAlertCobraHelper.MarkRequiredFlags(cmd.Flags(), globals.AlertVersion)
		if err != nil {
			return err
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		alertName := args[0]
		helmReleaseName := fmt.Sprintf("%s%s", alertName, globals.AlertPostSuffix)

		// Get the flags to set Helm values
		helmValuesMap, err := createAlertCobraHelper.GenerateHelmFlagsFromCobraFlags(cmd.Flags())
		if err != nil {
			return err
		}

		// Update the Helm Chart Location
		newChartVersion := "" // pass empty to UpdateHelmChartLocation if the default version should be used
		if cmd.Flags().Lookup("version").Changed {
			newChartVersion = cmd.Flags().Lookup("version").Value.String()
		}
		err = UpdateHelmChartLocation(cmd.Flags(), globals.AlertChartName, newChartVersion, &globals.AlertChartRepository)
		if err != nil {
			return fmt.Errorf("failed to set the app resources location due to %+v", err)
		}

		ok, err := util.IsNotDefaultVersionGreaterThanOrEqualTo(globals.AlertVersion, 5, 3, 1)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("creation of Alert instance is only suported for version 5.3.1 and above")
		}

		// Ensure helmValuesMap has the version set
		util.SetHelmValueInMap(helmValuesMap, []string{"version"}, globals.AlertVersion)

		// Get secrets for Alert
		certificateFlag := cmd.Flag("certificate-file-path")
		certificateKeyFlag := cmd.Flag("certificate-key-file-path")
		if certificateFlag.Changed && certificateKeyFlag.Changed {
			certificateData, err := util.ReadFileData(certificateFlag.Value.String())
			if err != nil {
				log.Fatalf("failed to read certificate file: %+v", err)
			}

			certificateKeyData, err := util.ReadFileData(certificateKeyFlag.Value.String())
			if err != nil {
				log.Fatalf("failed to read certificate file: %+v", err)
			}
			customCertificateSecretName := "alert-custom-certificate"
			customCertificateSecret := alert.GetAlertCustomCertificateSecret(namespace, customCertificateSecretName, certificateData, certificateKeyData)
			util.SetHelmValueInMap(helmValuesMap, []string{"webserverCustomCertificatesSecretName"}, customCertificateSecretName)
			fmt.Printf("---\n")
			if _, err = PrintComponent(customCertificateSecret, "YAML"); err != nil {
				return err
			}
		}

		javaKeystoreFlag := cmd.Flag("java-keystore-file-path")
		if javaKeystoreFlag.Changed {
			javaKeystoreData, err := util.ReadFileData(javaKeystoreFlag.Value.String())
			if err != nil {
				log.Fatalf("failed to read Java Keystore file: %+v", err)
			}
			javaKeystoreSecretName := "alert-java-keystore"
			javaKeystoreSecret := alert.GetAlertJavaKeystoreSecret(namespace, javaKeystoreSecretName, javaKeystoreData)
			util.SetHelmValueInMap(helmValuesMap, []string{"javaKeystoreSecretName"}, javaKeystoreSecretName)
			fmt.Printf("---\n")
			if _, err = PrintComponent(javaKeystoreSecret, "YAML"); err != nil {
				return err
			}
		}

		// Deploy Alert Resources
		err = util.TemplateWithHelm3(helmReleaseName, namespace, globals.AlertChartRepository, helmValuesMap)
		if err != nil {
			cleanErrorMsg := cleanAlertHelmError(err.Error(), helmReleaseName, alertName)
			return fmt.Errorf("failed to create Alert resources: %+v", cleanErrorMsg)
		}

		return nil
	},
}

/*
Create Black Duck Commands
*/

// createBlackDuckCmd creates a Black Duck instance
var createBlackDuckCmd = &cobra.Command{
	Use:           "blackduck NAME -n NAMESPACE",
	Example:       "synopsysctl create blackduck <name> -n <namespace>",
	Short:         "Create a Black Duck instance",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args: func(cmd *cobra.Command, args []string) error {
		// Check the Number of Arguments
		if len(args) != 1 {
			cmd.Help()
			return fmt.Errorf("this command takes 1 argument, but got %+v", args)
		}
		return nil
	},
	PreRunE: func(cmd *cobra.Command, args []string) error {
		// Set the Global BlackDuckVersion
		if cmd.Flags().Lookup("version").Changed {
			globals.BlackDuckVersion = cmd.Flags().Lookup("version").Value.String()
		}

		// Verify synopsysctl supports the version
		ok, err := util.IsVersionGreaterThanOrEqualTo(globals.BlackDuckVersion, 2020, time.April, 0)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("creation of Black Duck instance is only suported for version 2020.4.0 and above")
		}
		// Check the flags
		err = createBlackDuckCobraHelper.MarkRequiredFlags(cmd.Flags(), globals.BlackDuckVersion, true)
		if err != nil {
			return err
		}
		err = createBlackDuckCobraHelper.VerifyChartVersionSupportsChangedFlags(cmd.Flags(), globals.BlackDuckVersion)
		if err != nil {
			return err
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		// Set the Helm Chart Location
		newChartVersion := "" // pass empty to UpdateHelmChartLocation if the default version should be used
		if cmd.Flags().Lookup("version").Changed {
			newChartVersion = cmd.Flags().Lookup("version").Value.String() // note: globals.BlackDuckVersion is set in PreRunE
		}
		err := UpdateHelmChartLocation(cmd.Flags(), globals.BlackDuckChartName, newChartVersion, &globals.BlackDuckChartRepository)
		if err != nil {
			return fmt.Errorf("failed to set the app resources location due to %+v", err)
		}

		// Create Helm Chart Values
		helmValuesMap, err := createBlackDuckCobraHelper.GenerateHelmFlagsFromCobraFlags(cmd.Flags())
		if err != nil {
			return err
		}

		// Ensure helmValuesMap has the version set
		util.SetHelmValueInMap(helmValuesMap, []string{"version"}, globals.BlackDuckVersion)

		// Set HelmChart Value - isKubernetes to false in case of OpenShift
		if util.IsOpenshift(kubeClient) {
			util.SetHelmValueInMap(helmValuesMap, []string{"isKubernetes"}, false)
		}

		// Set Helm Chart Value - Persistent Storage to true by default (TODO: remove after changed in Helm Chart)
		if !cmd.Flag("persistent-storage").Changed {
			util.SetHelmValueInMap(helmValuesMap, []string{"enablePersistentStorage"}, true)
		}

		// Set Helm Chart Value - size
		var extraFiles []string
		if !cmd.Flags().Lookup("size").Changed {
			helmValuesMap["size"] = "small"
		}
		size, found := helmValuesMap["size"]
		if found && len(size.(string)) > 0 {
			yml, err := util.GetSizeYAMLFileName(size.(string), globals.BlackDuckVersion)
			if err != nil {
				return err
			}
			extraFiles = append(extraFiles, yml)
		}

		// Create initial resources
		secrets, err := blackduck.GetCertsFromFlagsAndSetHelmValue(args[0], namespace, cmd.Flags(), helmValuesMap)
		if err != nil {
			return err
		}
		for _, v := range secrets {
			if _, err := kubeClient.CoreV1().Secrets(namespace).Create(context.TODO(), &v, metav1.CreateOptions{}); err != nil && !k8serrors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create certifacte secret: %+v", err)
			}
		}

		// Check Dry Run before deploying any resources
		err = util.CreateWithHelm3(args[0], namespace, globals.BlackDuckChartRepository, helmValuesMap, kubeConfigPath, true, extraFiles...)
		if err != nil {
			return fmt.Errorf("failed to create Blackduck resources: %+v", err)
		}

		// Deploy Resources
		err = util.CreateWithHelm3(args[0], namespace, globals.BlackDuckChartRepository, helmValuesMap, kubeConfigPath, false, extraFiles...)
		if err != nil {
			return fmt.Errorf("failed to create Blackduck resources: %+v", err)
		}

		err = blackduck.CRUDServiceOrRoute(restconfig, kubeClient, namespace, args[0], helmValuesMap["exposeui"], helmValuesMap["exposedServiceType"], cmd.Flags().Lookup("expose-ui").Changed)
		if err != nil {
			return err
		}

		log.Infof("Black Duck has been successfully Created!")
		return nil
	},
}

// createBlackDuckNativeCmd prints the Kubernetes resources for creating a Black Duck instance
var createBlackDuckNativeCmd = &cobra.Command{
	Use:           "native NAME -n NAMESPACE",
	Example:       "synopsysctl create blackduck native <name> -n <namespace>",
	Short:         "Print the Kubernetes resources for creating a Black Duck instance",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args: func(cmd *cobra.Command, args []string) error {
		// Check the Number of Arguments
		if len(args) != 1 {
			cmd.Help()
			return fmt.Errorf("this command takes 1 argument, but got %+v", args)
		}
		return nil
	},
	PreRunE: func(cmd *cobra.Command, args []string) error {
		// Set the Global BlackDuckVersion
		if cmd.Flags().Lookup("version").Changed {
			globals.BlackDuckVersion = cmd.Flags().Lookup("version").Value.String()
		}

		// Verify synopsysctl supports the version
		ok, err := util.IsVersionGreaterThanOrEqualTo(globals.BlackDuckVersion, 2020, time.April, 0)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("creation of Black Duck instance is only suported for version 2020.4.0 and above")
		}
		// Check the flags
		err = createBlackDuckCobraHelper.MarkRequiredFlags(cmd.Flags(), globals.BlackDuckVersion, true)
		if err != nil {
			return err
		}
		err = createBlackDuckCobraHelper.VerifyChartVersionSupportsChangedFlags(cmd.Flags(), globals.BlackDuckVersion)
		if err != nil {
			return err
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		// Set the Helm Chart Location
		newChartVersion := "" // pass empty to UpdateHelmChartLocation if the default version should be used
		if cmd.Flags().Lookup("version").Changed {
			newChartVersion = cmd.Flags().Lookup("version").Value.String() // note: globals.BlackDuckVersion is set in PreRunE
		}
		err := UpdateHelmChartLocation(cmd.Flags(), globals.BlackDuckChartName, newChartVersion, &globals.BlackDuckChartRepository)
		if err != nil {
			return fmt.Errorf("failed to set the app resources location due to %+v", err)
		}

		// Create Helm Chart Values
		helmValuesMap, err := createBlackDuckCobraHelper.GenerateHelmFlagsFromCobraFlags(cmd.Flags())
		if err != nil {
			return err
		}

		// Ensure helmValuesMap has the version set
		util.SetHelmValueInMap(helmValuesMap, []string{"version"}, globals.BlackDuckVersion)

		// Set Helm Chart Value - Check if the configuration is for Openshift
		err = verifyClusterType(globals.NativeClusterType)
		if err != nil {
			return fmt.Errorf("invalid cluster type '%s'", globals.NativeClusterType)
		}
		if strings.ToUpper(globals.NativeClusterType) == globals.ClusterTypeOpenshift {
			util.SetHelmValueInMap(helmValuesMap, []string{"isKubernetes"}, false)
		} else {
			util.SetHelmValueInMap(helmValuesMap, []string{"isKubernetes"}, true)
		}

		// Set Helm Chart Value - Persistent Storage to true by default (TODO: remove after changed in Helm Chart)
		if !cmd.Flag("persistent-storage").Changed {
			util.SetHelmValueInMap(helmValuesMap, []string{"enablePersistentStorage"}, true)
		}

		// Set Helm Chart Value - size
		var extraFiles []string
		if !cmd.Flags().Lookup("size").Changed {
			helmValuesMap["size"] = "small"
		}
		size, found := helmValuesMap["size"]
		if found && len(size.(string)) > 0 {
			yml, err := util.GetSizeYAMLFileName(size.(string), globals.BlackDuckVersion)
			if err != nil {
				return err
			}
			extraFiles = append(extraFiles, yml)
		}

		// Create initial resources
		secrets, err := blackduck.GetCertsFromFlagsAndSetHelmValue(args[0], namespace, cmd.Flags(), helmValuesMap)
		if err != nil {
			return err
		}
		for _, v := range secrets {
			fmt.Printf("---\n")
			PrintComponent(v, "YAML") // helm only supports yaml
		}

		// Print the resources
		err = util.TemplateWithHelm3(args[0], namespace, globals.BlackDuckChartRepository, helmValuesMap, extraFiles...)
		if err != nil {
			return fmt.Errorf("failed to create Blackduck resources: %+v", err)
		}

		return nil
	},
}

/*
Create OpsSight Commands
*/

// createOpsSightCmd creates an OpsSight instance
var createOpsSightCmd = &cobra.Command{
	Use:           "opssight NAME -n NAMESPACE",
	Example:       "synopsysctl create opssight <name> -n <namespace>",
	Short:         "Create an OpsSight instance",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args: func(cmd *cobra.Command, args []string) error {
		// Check the Number of Arguments
		if len(args) != 1 {
			cmd.Help()
			return fmt.Errorf("this command takes 1 argument, but got %+v", args)
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		opssightName := args[0]

		// Get the flags to set Helm values
		helmValuesMap, err := createOpsSightCobraHelper.GenerateHelmFlagsFromCobraFlags(cmd.Flags())
		if err != nil {
			return err
		}

		// Update the Helm Chart Location
		newChartVersion := "" // pass empty to UpdateHelmChartLocation if the default version should be used
		if cmd.Flags().Lookup("version").Changed {
			globals.OpsSightVersion = cmd.Flags().Lookup("version").Value.String()
			newChartVersion = globals.OpsSightVersion
		}
		err = UpdateHelmChartLocation(cmd.Flags(), globals.OpsSightChartName, newChartVersion, &globals.OpsSightChartRepository)
		if err != nil {
			return fmt.Errorf("failed to set the app resources location due to %+v", err)
		}

		// Set the version in the Values
		util.SetHelmValueInMap(helmValuesMap, []string{"version"}, globals.OpsSightVersion)

		// Check Dry Run before deploying any resources
		err = util.CreateWithHelm3(opssightName, namespace, globals.OpsSightChartRepository, helmValuesMap, kubeConfigPath, true)
		if err != nil {
			return fmt.Errorf("failed to create OpsSight resources: %+v", err)
		}

		// Deploy OpsSight Resources
		err = util.CreateWithHelm3(opssightName, namespace, globals.OpsSightChartRepository, helmValuesMap, kubeConfigPath, false)
		if err != nil {
			return fmt.Errorf("failed to create OpsSight resources: %+v", err)
		}

		log.Infof("OpsSight has been successfully Created!")
		return nil
	},
}

// createOpsSightNativeCmd prints the Kubernetes resources for creating an OpsSight instance
var createOpsSightNativeCmd = &cobra.Command{
	Use:           "native NAME -n NAMESPACE",
	Example:       "synopsysctl create opssight native <name> -n <namespace>",
	Short:         "Print the Kubernetes resources for creating an OpsSight instance",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args: func(cmd *cobra.Command, args []string) error {
		// Check the Number of Arguments
		if len(args) != 1 {
			cmd.Help()
			return fmt.Errorf("this command takes 1 argument, but got %+v", args)
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		opssightName := args[0]

		// Get the flags to set Helm values
		helmValuesMap, err := createOpsSightCobraHelper.GenerateHelmFlagsFromCobraFlags(cmd.Flags())
		if err != nil {
			return err
		}

		// Update the Helm Chart Location
		newChartVersion := "" // pass empty to UpdateHelmChartLocation if the default version should be used
		if cmd.Flags().Lookup("version").Changed {
			globals.OpsSightVersion = cmd.Flags().Lookup("version").Value.String()
			newChartVersion = globals.OpsSightVersion
		}
		err = UpdateHelmChartLocation(cmd.Flags(), globals.OpsSightChartName, newChartVersion, &globals.OpsSightChartRepository)
		if err != nil {
			return fmt.Errorf("failed to set the app resources location due to %+v", err)
		}

		// Set the version in the Values
		util.SetHelmValueInMap(helmValuesMap, []string{"version"}, globals.OpsSightVersion)

		// Print OpsSight Resources
		err = util.TemplateWithHelm3(opssightName, namespace, globals.OpsSightChartRepository, helmValuesMap)
		if err != nil {
			return fmt.Errorf("failed to generate OpsSight resources: %+v", err)
		}

		return nil
	},
}

// createBDBACmd creates a BDBA instance
var createBDBACmd = &cobra.Command{
	Use:           "bdba -n NAMESPACE",
	Example:       "synopsysctl create bdba -n <namespace>",
	Short:         "Create a BDBA instance",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args: func(cmd *cobra.Command, args []string) error {
		// Check the Number of Arguments
		if len(args) != 0 {
			cmd.Help()
			return fmt.Errorf("this command takes 0 arguments, but got %+v", args)
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		// Get the flags to set Helm values
		helmValuesMap, err := createBDBACobraHelper.GenerateHelmFlagsFromCobraFlags(cmd.Flags())
		if err != nil {
			return err
		}

		// Update the Helm Chart Location
		newChartVersion := "" // pass empty to UpdateHelmChartLocation if the default version should be used
		if cmd.Flags().Lookup("version").Changed {
			globals.BDBAVersion = cmd.Flags().Lookup("version").Value.String()
			newChartVersion = globals.BDBAVersion
		}
		err = UpdateHelmChartLocation(cmd.Flags(), globals.BDBAChartName, newChartVersion, &globals.BDBAChartRepository)
		if err != nil {
			return fmt.Errorf("failed to set the app resources location due to %+v", err)
		}

		// Set the version in the Values
		util.SetHelmValueInMap(helmValuesMap, []string{"version"}, globals.BDBAVersion)

		// Check Dry Run before deploying any resources
		err = util.CreateWithHelm3(globals.BDBAName, namespace, globals.BDBAChartRepository, helmValuesMap, kubeConfigPath, true)
		if err != nil {
			return fmt.Errorf("failed to create BDBA resources: %+v", err)
		}

		// Deploy Resources
		err = util.CreateWithHelm3(globals.BDBAName, namespace, globals.BDBAChartRepository, helmValuesMap, kubeConfigPath, false)
		if err != nil {
			return fmt.Errorf("failed to create BDBA resources: %+v", err)
		}

		log.Infof("BDBA has been successfully Created!")
		return nil
	},
}

// createBDBANativeCmd prints BDBA resources
var createBDBANativeCmd = &cobra.Command{
	Use:           "native -n NAMESPACE",
	Example:       "synopsysctl create bdba -n <namespace>",
	Short:         "Print Kubernetes resources for creating a BDBA instance",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args: func(cmd *cobra.Command, args []string) error {
		// Check the Number of Arguments
		if len(args) != 0 {
			cmd.Help()
			return fmt.Errorf("this command takes 0 arguments, but got %+v", args)
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		// Get the flags to set Helm values
		helmValuesMap, err := createBDBACobraHelper.GenerateHelmFlagsFromCobraFlags(cmd.Flags())
		if err != nil {
			return err
		}

		// Update the Helm Chart Location
		newChartVersion := "" // pass empty to UpdateHelmChartLocation if the default version should be used
		if cmd.Flags().Lookup("version").Changed {
			globals.BDBAVersion = cmd.Flags().Lookup("version").Value.String()
			newChartVersion = globals.BDBAVersion
		}
		err = UpdateHelmChartLocation(cmd.Flags(), globals.BDBAChartName, newChartVersion, &globals.BDBAChartRepository)
		if err != nil {
			return fmt.Errorf("failed to set the app resources location due to %+v", err)
		}

		// Set the version in the Values
		util.SetHelmValueInMap(helmValuesMap, []string{"version"}, globals.BDBAVersion)

		// Print Resources
		err = util.TemplateWithHelm3(globals.BDBAName, namespace, globals.BDBAChartRepository, helmValuesMap)
		if err != nil {
			return fmt.Errorf("failed to generate BDBA resources: %+v", err)
		}

		return nil
	},
}

func init() {
	// initialize global resource ctl structs for commands to use
	createBlackDuckCobraHelper = *blackduck.NewHelmValuesFromCobraFlags()
	createAlertCobraHelper = *alertctl.NewHelmValuesFromCobraFlags()
	createOpsSightCobraHelper = *opssight.NewHelmValuesFromCobraFlags()
	createBDBACobraHelper = *bdba.NewHelmValuesFromCobraFlags()

	rootCmd.AddCommand(createCmd)

	// Add Alert Command
	createAlertCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", namespace, "Namespace of the instance(s)")
	cobra.MarkFlagRequired(createAlertCmd.PersistentFlags(), "namespace")
	createAlertCobraHelper.AddCobraFlagsToCommand(createAlertCmd, true)
	addChartLocationPathFlag(createAlertCmd)
	createCmd.AddCommand(createAlertCmd)

	createAlertCobraHelper.AddCobraFlagsToCommand(createAlertNativeCmd, true)
	addChartLocationPathFlag(createAlertNativeCmd)
	createAlertCmd.AddCommand(createAlertNativeCmd)

	// Add Black Duck Command
	createBlackDuckCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", namespace, "Namespace of the instance(s)")
	cobra.MarkFlagRequired(createBlackDuckCmd.PersistentFlags(), "namespace")
	addChartLocationPathFlag(createBlackDuckCmd)
	createBlackDuckCobraHelper.AddCobraFlagsToCommand(createBlackDuckCmd, true)
	createCmd.AddCommand(createBlackDuckCmd)

	createBlackDuckCobraHelper.AddCobraFlagsToCommand(createBlackDuckNativeCmd, true)
	addNativeFlags(createBlackDuckNativeCmd)
	addChartLocationPathFlag(createBlackDuckNativeCmd)
	createBlackDuckCmd.AddCommand(createBlackDuckNativeCmd)

	// Add OpsSight Command
	createOpsSightCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", namespace, "Namespace of the instance(s)")
	cobra.MarkFlagRequired(createOpsSightCmd.PersistentFlags(), "namespace")
	addChartLocationPathFlag(createOpsSightCmd)
	createOpsSightCobraHelper.AddCobraFlagsToCommand(createOpsSightCmd, true)
	createCmd.AddCommand(createOpsSightCmd)

	createOpsSightCobraHelper.AddCobraFlagsToCommand(createOpsSightNativeCmd, true)
	addChartLocationPathFlag(createOpsSightNativeCmd)
	createOpsSightCmd.AddCommand(createOpsSightNativeCmd)

	// Add BDBA commands
	createBDBACmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", namespace, "Namespace of the instance(s)")
	cobra.MarkFlagRequired(createBDBACmd.PersistentFlags(), "namespace")
	createBDBACobraHelper.AddCobraFlagsToCommand(createBDBACmd, true)
	addChartLocationPathFlag(createBDBACmd)
	createCmd.AddCommand(createBDBACmd)

	createBDBACobraHelper.AddCobraFlagsToCommand(createBDBANativeCmd, true)
	addChartLocationPathFlag(createBDBANativeCmd)
	createBDBACmd.AddCommand(createBDBANativeCmd)

}
