package main

import (
	iconfig "github.com/rpajarola/mirrortools/internal/config"
)

type config = iconfig.Config
type methodConfig = iconfig.MethodConfig

func configPath() string {
	return iconfig.Path()
}

func loadConfig(path string) (*config, error) {
	return iconfig.Load(path)
}
