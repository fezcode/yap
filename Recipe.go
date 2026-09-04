//go:build gobake

package bake_recipe

import (
	"fmt"

	"github.com/fezcode/gobake"
)

func Run(bake *gobake.Engine) error {
	if err := bake.LoadRecipeInfo("recipe.piml"); err != nil {
		return err
	}

	bake.Task("build", "Builds the binary for multiple platforms", func(ctx *gobake.Context) error {
		ctx.Log("Building %s v%s...", bake.Info.Name, bake.Info.Version)

		targets := []struct {
			os   string
			arch string
		}{
			{"linux", "amd64"},
			{"linux", "arm64"},
			{"windows", "amd64"},
			{"windows", "arm64"},
			{"darwin", "amd64"},
			{"darwin", "arm64"},
		}

		err := ctx.Mkdir("build")
		if err != nil {
			return err
		}

		ldflags := fmt.Sprintf("-X main.Version=%s", bake.Info.Version)

		for _, t := range targets {
			// The audio backend on Linux is ALSA through oto, which needs cgo, so this target cannot be cross-compiled with
			// CGO disabled. Build it on a Linux machine instead of failing here.
			if t.os == "linux" {
				ctx.Log("Skipping %s/%s: ALSA needs cgo (build on Linux)", t.os, t.arch)
				continue
			}

			output := "build/" + bake.Info.Name + "-" + t.os + "-" + t.arch
			if t.os == "windows" {
				output += ".exe"
			}

			ctx.Env = []string{
				"CGO_ENABLED=0",
				"GOOS=" + t.os,
				"GOARCH=" + t.arch,
			}

			// We use manual go build to inject ldflags
			err := ctx.Run("go", "build", "-ldflags", ldflags, "-o", output, "./src")
			if err != nil {
				return err
			}
		}
		return nil
	})

	bake.Task("clean", "Removes build artifacts", func(ctx *gobake.Context) error {
		return ctx.Remove("build")
	})

	return nil
}
