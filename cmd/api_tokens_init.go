package cmd

import (
	"fmt"
	"log"
	"os"

	"github.com/provideservices/provide-go"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var apiTokensInitCmd = &cobra.Command{
	Use:   "init --application 8fec625c-a8ad-4197-bb77-8b46d7aecd8f",
	Short: "Creates a new API token",
	Long:  `Initialize a new application API Token`,
	Run:   createAPIToken,
}

// createAPIToken triggers the generation of an API token for the given network.
func createAPIToken(cmd *cobra.Command, args []string) {
	token := requireUserAuthToken()
	params := map[string]interface{}{}
	status, resp, err := provide.CreateApplicationToken(token, applicationID, params)
	if err != nil {
		log.Printf("Failed to create API token; %s", err.Error())
		os.Exit(1)
	}
	if status == 201 {
		apiToken := resp.(map[string]interface{})
		appAPITokenKey := buildConfigKeyWithApp(apiTokenConfigKeyPartial, applicationID)
		if !viper.IsSet(appAPITokenKey) {
			viper.Set(appAPITokenKey, apiToken["token"])
			viper.WriteConfig()
		}
		fmt.Printf("API Token\t%s\n", apiToken["token"])
	} else {
		fmt.Printf("Failed to create API token; %s", resp)
		os.Exit(1)
	}
}

func init() {
	apiTokensInitCmd.Flags().StringVar(&applicationID, "application", "", "application id")
	apiTokensInitCmd.MarkFlagRequired("application")
}
