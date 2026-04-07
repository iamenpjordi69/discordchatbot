package main

import (
	"fmt"
	"log"
	"os"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
)

func main() {
	godotenv.Load()
	token := os.Getenv("DISCORD_BOT_TOKEN")
	if token == "" {
		log.Fatal("DISCORD_BOT_TOKEN is missing")
	}

	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Fatal(err)
	}

	appID := "" // Will be fetched from state
	err = dg.Open()
	if err != nil {
		log.Fatal(err)
	}
	defer dg.Close()

	appID = dg.State.User.ID
	fmt.Printf("Registering commands for Bot ID: %s\n", appID)

	guildInstall := discordgo.ApplicationIntegrationType(0)
	userInstall := discordgo.ApplicationIntegrationType(1)
	guildContext := discordgo.InteractionContextType(0)
	dmContext := discordgo.InteractionContextType(1)
	privateContext := discordgo.InteractionContextType(2)

	commands := []*discordgo.ApplicationCommand{
		{
			Name:        "ask",
			Description: "Ask the AI a question (Works in DMs and Servers)",
			IntegrationTypes: &[]discordgo.ApplicationIntegrationType{
				guildInstall,
				userInstall,
			},
			Contexts: &[]discordgo.InteractionContextType{
				guildContext,
				dmContext,
				privateContext,
			},
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "question",
					Description: "Your question for the AI",
					Required:    true,
				},
			},
		},
	}

	_, err = dg.ApplicationCommandBulkOverwrite(appID, "", commands)
	if err != nil {
		log.Fatalf("Command Sync Error: %v", err)
	}
	fmt.Println("✅ Successfully registered Slash Commands globally.")
}
