package cmd

import (
	"database/sql"
	"fmt"
	"strings"

	_ "github.com/lib/pq"
	"github.com/spf13/cobra"

	"cli_app/internal/db"
)

// enumFlag implements pflag.Value to restrict a flag to a fixed set of choices.
type enumFlag struct {
	allowed []string
	value   string
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
	datapackHost       string
	datapackPort       int
	datapackName  string
	datapackEmbedModel = newEnumFlag(allowedEmbedModels)
	datapackInferModel = newEnumFlag(allowedInferModels)
	datapackKbase      string
)

var datapackCmd = &cobra.Command{
	Use:   "datapack",
	Short: "Manage datapacks",
	Long:  `Manage datapacks`,
}

var datapackCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new datapack for an agent",
	Long:  `Create a new datapack by registering an agent with its short-term, long-term memory, and knowledge base configuration.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if datapackName == "" {
			return fmt.Errorf("--datapack_name is required")
		}
		if datapackEmbedModel.value == "" {
			return fmt.Errorf("--embed_model is required (options: %s)", strings.Join(allowedEmbedModels, ", "))
		}
		if datapackInferModel.value == "" {
			return fmt.Errorf("--inference_model is required (options: %s)", strings.Join(allowedInferModels, ", "))
		}
		if datapackKbase == "" {
			return fmt.Errorf("--knowledge_base is required")
		}

		connStr := fmt.Sprintf(
			"postgresql://%s:%d/yugabyte?user=yugabyte&password=&sslmode=disable",
			datapackHost, datapackPort,
		)

		conn, err := sql.Open("postgres", connStr)
		if err != nil {
			return fmt.Errorf("failed to connect to Meko Platform: %w", err)
		}
		defer conn.Close()

		if err := conn.Ping(); err != nil {
			return fmt.Errorf("failed to connect to Meko Platform %s:%d: %w", datapackHost, datapackPort, err)
		}

		defaultAgentName := "meko_user_agent"
		datapackID, agentID, err := db.CreateDatapack(conn, datapackPort, datapackHost, datapackName, defaultAgentName, datapackEmbedModel.value, datapackInferModel.value, datapackKbase)
		if err != nil {
			return fmt.Errorf("failed to create datapack: %w", err)
		}
		
		fmt.Printf("Datapack %s created successfully\n", datapackName)
		fmt.Printf("  Datapack Id    : %s\n", datapackID)
		fmt.Printf("  Agent Name     : %s\n", defaultAgentName)
		fmt.Printf("  Agent Id       : %s\n", agentID)
		fmt.Printf("  URL to connect : https://localhost:5435/mcp/%s\n", datapackName)
		fmt.Printf("  Datapack Configuration:\n")
		fmt.Printf("    - Knowledge Base : %s\n", datapackKbase)
		fmt.Printf("    - Embed Model    : %s\n", datapackEmbedModel.value)
		fmt.Printf("    - Inference Model: %s\n", datapackInferModel.value)

		return nil
	},
}

var datapackListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all datapacks",
	Long:  `List all datapacks`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return nil
	},
}

var datapackDeleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Delete a datapack",
	Long:  `Delete a datapack`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return nil
	},
}

var datapackConnectCmd = &cobra.Command{
	Use:   "connect",
	Short: "Connect to a datapack",
	Long:  `Connect to a datapack`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return nil
	},
}

func init() {
	rootCmd.AddCommand(datapackCmd)
	datapackCmd.AddCommand(datapackCreateCmd)
	datapackCmd.AddCommand(datapackListCmd)
	datapackCmd.AddCommand(datapackDeleteCmd)
	datapackCmd.AddCommand(datapackConnectCmd)	

	datapackCmd.PersistentFlags().StringVar(&datapackHost, "host", "127.0.0.1", "database host")
	datapackCmd.PersistentFlags().IntVar(&datapackPort, "port", 5433, "database port")

	datapackCreateCmd.Flags().StringVar(&datapackName, "name", "", "name of the agent (required)")
	datapackCreateCmd.Flags().Var(datapackEmbedModel, "embed_model", "embedding model to use (required)")
	datapackCreateCmd.Flags().Var(datapackInferModel, "inference_model", "inference model to use (required)")
	datapackCreateCmd.Flags().StringVar(&datapackKbase, "seed_knowledge_base", "", "Directory containing the knowledge base (required)")

	datapackCreateCmd.RegisterFlagCompletionFunc("embed_model", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return allowedEmbedModels, cobra.ShellCompDirectiveNoFileComp
	})
	datapackCreateCmd.RegisterFlagCompletionFunc("inference_model", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return allowedInferModels, cobra.ShellCompDirectiveNoFileComp
	})
}
