// Copyright 2014 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"github.com/tsuru/config"
	"github.com/tsuru/tsuru/api"
	"github.com/tsuru/tsuru/cmd"
	"launchpad.net/gnuflag"
)

type apiCmd struct {
	fs    *gnuflag.FlagSet
	dry   bool
	check bool
}

func (c *apiCmd) Run(context *cmd.Context, client *cmd.Client) error {
	if c.check {
		err := config.Check([]config.Checker{CheckProvisioner})
		if err != nil {
			return err
		}
	}
	api.RunServer(c.dry)
	return nil
}

func (apiCmd) Info() *cmd.Info {
	return &cmd.Info{
		Name:    "api",
		Usage:   "api",
		Desc:    "Starts the tsuru api webserver.",
		MinArgs: 0,
	}
}

func (c *apiCmd) Flags() *gnuflag.FlagSet {
	if c.fs == nil {
		c.fs = gnuflag.NewFlagSet("api", gnuflag.ExitOnError)
		c.fs.BoolVar(&c.dry, "dry", false, "dry-run: does not start the server (for testing purpose)")
		c.fs.BoolVar(&c.dry, "d", false, "dry-run: does not start the server (for testing purpose)")
		c.fs.BoolVar(&c.check, "t", false, "check config: test your tsuru.conf file before starts.")
	}
	return c.fs
}
