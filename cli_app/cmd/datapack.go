package cmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"cli_app/internal/apiclient"
)

// enumFlag implements pflag.Value to restrict a flag to a fixed set of choices.
type enumFlag struct {
	value   string
	allowed []string
}

func newEnumFlag(allowed []string) *enumFlag {
	return &enumFlag{allowed: allowed}
}

func (e *enumFlag) String() string { return e.value }

func (e *enumFlag) Set(val string) error {
	for _, a := range e.allowed {
		if val == a {
			e.value = val
			return nil
		}
	}
	return fmt.Errorf("must be one of: %s", strings.Join(e.allowed, ", "))
}

func (e *enumFlag) Type() string { return "string" }

var allowedEmbedModels = []string{
	"text-embedding-ada-002",
	"text-embedding-3-small",
	"text-embedding-3-large",
}

var allowedInferModels = []string{
	"GPT-4o",
	"LLama-3.1-8B-Instruct",
	"DeepSeek-R1",
}

var (
	apiHost            string
	apiPort            int
	datapackName       string
	datapackEmbedModel = newEnumFlag(allowedEmbedModels)
	datapackInferModel = newEnumFlag(allowedInferModels)
	datapackKbase      string
)

func newAPIClient() *apiclient.Client {
	return apiclient.New(apiHost, apiPort)
}

func strVal(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func numVal(m map[string]any, key string) float64 {
	if v, ok := m[key].(float64); ok {
		return v
	}
	return 0
}

var datapackCmd = &cobra.Command{
	Use:   "datapack",
	Short: "Manage datapacks",
	Long:  `Manage datapacks via the meko API server.`,
}

var datapackCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new datapack for an agent",
	Long:  `Create a new datapack by registering an agent with its knowledge base configuration.`,
	RunE: func(_ *cobra.Command, _ []string) error {
		if datapackName == "" {
			return fmt.Errorf("--name is required")
		}
		if datapackKbase == "" {
			return fmt.Errorf("--seed_knowledge_base is required")
		}

		data, err := newAPIClient().CreateDatapack(
			datapackName, datapackKbase,
			datapackEmbedModel.value, datapackInferModel.value,
		)
		if err != nil {
			return fmt.Errorf("failed to create datapack: %w", err)
		}

		var result map[string]string
		if err := json.Unmarshal(data, &result); err != nil {
			return fmt.Errorf("unexpected response: %w", err)
		}

		fmt.Printf("Datapack %s created successfully\n", datapackName)
		fmt.Printf("  Datapack Id    : %s\n", result["datapack_id"])
		fmt.Printf("  Agent Id       : %s\n", result["agent_id"])
		if datapackKbase != "" {
			fmt.Printf("  Knowledge Base : %s\n", datapackKbase)
		}
		if datapackEmbedModel.value != "" {
			fmt.Printf("  Embed Model    : %s\n",
				datapackEmbedModel.value)
		}
		if datapackInferModel.value != "" {
			fmt.Printf("  Inference Model: %s\n",
				datapackInferModel.value)
		}
		return nil
	},
}

var datapackListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all datapacks",
	RunE: func(_ *cobra.Command, _ []string) error {
		data, err := newAPIClient().ListDatapacks()
		if err != nil {
			return fmt.Errorf("failed to list datapacks: %w", err)
		}

		var datapacks []map[string]any
		if err := json.Unmarshal(data, &datapacks); err != nil {
			return fmt.Errorf("unexpected response: %w", err)
		}

		if len(datapacks) == 0 {
			fmt.Println("No datapacks found.")
			return nil
		}

		fmt.Printf("%-30s %-36s %-20s\n", "NAME", "ID", "CREATED")
		for _, dp := range datapacks {
			created := strVal(dp, "created_at")
			if len(created) > 19 {
				created = created[:19]
			}
			fmt.Printf("%-30s %-36s %-20s\n",
				strVal(dp, "name"),
				strVal(dp, "id"),
				created)
		}
		return nil
	},
}

var datapackDeleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Delete a datapack",
	RunE: func(_ *cobra.Command, _ []string) error {
		if datapackName == "" {
			return fmt.Errorf("--name is required")
		}

		if _, err := newAPIClient().DeleteDatapack(datapackName); err != nil {
			return fmt.Errorf("failed to delete datapack: %w", err)
		}

		fmt.Printf("Datapack %q deleted.\n", datapackName)
		return nil
	},
}

var datapackDescribeCmd = &cobra.Command{
	Use:   "describe",
	Short: "Describe a datapack",
	RunE: func(_ *cobra.Command, _ []string) error {
		if datapackName == "" {
			return fmt.Errorf("--name is required")
		}

		data, err := newAPIClient().DescribeDatapack(datapackName)
		if err != nil {
			return fmt.Errorf("failed to describe datapack: %w", err)
		}

		var dp map[string]any
		if err := json.Unmarshal(data, &dp); err != nil {
			return fmt.Errorf("unexpected response: %w", err)
		}

		fmt.Printf("Name            : %s\n", strVal(dp, "name"))
		fmt.Printf("ID              : %s\n", strVal(dp, "id"))
		fmt.Printf("LLM Model       : %s\n", strVal(dp, "llm_model"))
		fmt.Printf("Embedding Model : %s\n",
			strVal(dp, "embedding_model"))
		fmt.Printf("Created         : %s\n",
			strVal(dp, "created_at"))
		return nil
	},
}

var datapackStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show datapack status",
	RunE: func(_ *cobra.Command, _ []string) error {
		if datapackName == "" {
			return fmt.Errorf("--name is required")
		}

		data, err := newAPIClient().DatapackStatus(datapackName)
		if err != nil {
			return fmt.Errorf("failed to get status: %w", err)
		}

		var st map[string]any
		if err := json.Unmarshal(data, &st); err != nil {
			return fmt.Errorf("unexpected response: %w", err)
		}

		fmt.Printf("Datapack        : %s\n", strVal(st, "name"))
		fmt.Printf("ID              : %s\n", strVal(st, "id"))
		fmt.Printf("LLM Model       : %s\n", strVal(st, "llm_model"))
		fmt.Printf("Embedding Model : %s\n",
			strVal(st, "embedding_model"))
		fmt.Printf("Agent Count     : %.0f\n",
			numVal(st, "agent_count"))
		fmt.Printf("KB Count        : %.0f\n",
			numVal(st, "knowledge_base_count"))
		fmt.Printf("Created         : %s\n",
			strVal(st, "created_at"))
		return nil
	},
}

var datapackConnectCmd = &cobra.Command{
	Use:   "connect",
	Short: "Connect to a datapack",
	RunE: func(_ *cobra.Command, _ []string) error {
		if datapackName == "" {
			return fmt.Errorf("--name is required")
		}

		if _, err := newAPIClient().DescribeDatapack(datapackName); err != nil {
			return fmt.Errorf("datapack not reachable: %w", err)
		}

		fmt.Printf("Connected to datapack %q\n", datapackName)
		fmt.Printf("  MCP endpoint: https://localhost:5435/mcp/%s\n",
			datapackName)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(datapackCmd)
	datapackCmd.AddCommand(datapackCreateCmd)
	datapackCmd.AddCommand(datapackListCmd)
	datapackCmd.AddCommand(datapackDeleteCmd)
	datapackCmd.AddCommand(datapackDescribeCmd)
	datapackCmd.AddCommand(datapackStatusCmd)
	datapackCmd.AddCommand(datapackConnectCmd)

	datapackCmd.PersistentFlags().StringVar(
		&apiHost, "api_host", "127.0.0.1", "API server host")
	datapackCmd.PersistentFlags().IntVar(
		&apiPort, "api_port", 25433, "API server port")

	datapackCreateCmd.Flags().StringVar(
		&datapackName, "name", "", "datapack name (required)")
	datapackCreateCmd.Flags().Var(
		datapackEmbedModel, "embed_model", "embedding model")
	datapackCreateCmd.Flags().Var(
		datapackInferModel, "inference_model", "inference model")
	datapackCreateCmd.Flags().StringVar(
		&datapackKbase, "seed_knowledge_base", "",
		"knowledge base URL (required)")

	for _, sub := range []*cobra.Command{
		datapackDeleteCmd, datapackDescribeCmd,
		datapackStatusCmd, datapackConnectCmd,
	} {
		sub.Flags().StringVar(
			&datapackName, "name", "", "datapack name (required)")
	}

	//nolint:errcheck // flag completions are best-effort
	datapackCreateCmd.RegisterFlagCompletionFunc("embed_model",
		func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
			return allowedEmbedModels, cobra.ShellCompDirectiveNoFileComp
		})
	//nolint:errcheck // flag completions are best-effort
	datapackCreateCmd.RegisterFlagCompletionFunc("inference_model",
		func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
			return allowedInferModels, cobra.ShellCompDirectiveNoFileComp
		})
}
