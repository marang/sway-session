package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

func main() {
	replace := flag.Bool("replace", false, "replace an existing tab title daemon")
	preset := flag.String("preset", envDefault("SWAY_TAB_ANIMATION", animationPreset), "animation preset (default: configured rotation)")
	fps := flag.Float64("fps", 0, "animation frames per second")
	configPath := flag.String("config", envDefault("SWAY_TITLE_ANIMATOR_CONFIG", defaultConfigPath()), "TOML config path")
	initConfigFlag := flag.Bool("init-config", false, "write an example config if it does not exist")
	list := flag.Bool("list-presets", false, "list animation presets")
	preview := flag.Bool("preview", false, "preview all animations in the terminal")
	socket := flag.String("socket", os.Getenv("SWAYSOCK"), "sway IPC socket")
	flag.Parse()

	if *initConfigFlag {
		if err := initConfig(*configPath); err != nil {
			fmt.Fprintf(os.Stderr, "Unable to initialize config %s: %v\n", *configPath, err)
			os.Exit(1)
		}
		fmt.Printf("Created %s\n", *configPath)
		return
	}

	if err := loadConfig(*configPath); err != nil {
		fmt.Fprintf(os.Stderr, "Unable to load config %s: %v\n", *configPath, err)
		os.Exit(2)
	}
	if *fps > 0 {
		settings.FPS = *fps
	} else if envFPS := os.Getenv("SWAY_TAB_FPS"); envFPS != "" {
		if parsed, err := strconv.ParseFloat(envFPS, 64); err == nil {
			settings.FPS = parsed
		}
	}
	if *list {
		listPresets()
		return
	}
	if *preview {
		if err := validateSettings(settings); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if err := runPreview(os.Stdout, settings.FPS); err != nil {
			fmt.Fprintf(os.Stderr, "Unable to preview animations: %v\n", err)
			os.Exit(2)
		}
		return
	}
	if *preset != rotationSelection {
		if _, ok := animationPresets[*preset]; !ok {
			fmt.Fprintf(os.Stderr, "Unknown animation preset: %s\n", *preset)
			fmt.Fprintf(os.Stderr, "Available:\n")
			listPresets()
			os.Exit(2)
		}
	}
	animationPreset = *preset
	if err := validateSettings(settings); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if *socket == "" {
		fmt.Fprintln(os.Stderr, "SWAYSOCK is not set; pass --socket or start from Sway")
		os.Exit(2)
	}

	lockFile, err := runtimeFile()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to prepare instance lock: %v\n", err)
		os.Exit(1)
	}
	lock, err := acquireInstanceLock(lockFile, *replace)
	if errors.Is(err, errInstanceRunning) {
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to acquire instance lock: %v\n", err)
		os.Exit(1)
	}
	exitCode := runLoopWithFPS(*socket, settings.FPS)
	if err := lock.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "Unable to release instance lock: %v\n", err)
		if exitCode == 0 {
			exitCode = 1
		}
	}
	os.Exit(exitCode)
}

func defaultConfigPath() string {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		configHome = filepath.Join(home, ".config")
	}
	return filepath.Join(configHome, "sway-title-animator", "config.toml")
}

func envDefault(name string, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
