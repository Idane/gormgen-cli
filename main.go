package main

import (
	"fmt"
	"os"
	"runtime/debug"

	"github.com/Idane/gormgen-cli/internal/gen"
	"github.com/spf13/cobra"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "gorm2",
		Short: "GORM CLI Tool",
	}

	rootCmd.AddCommand(gen.New())
	rootCmd.AddCommand(versionCmd())

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version of gorm-cli",
		Run: func(cmd *cobra.Command, args []string) {
			if info, ok := debug.ReadBuildInfo(); ok {
				if info.Main.Version != "" && info.Main.Version != "(devel)" {
					fmt.Printf("gorm-cli version %s\n", info.Main.Version)
					return
				}
			}

			fmt.Printf("gorm-cli version %s\n", "dev")
		},
	}
}
