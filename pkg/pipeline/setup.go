// SPDX-FileCopyrightText: 2023 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"text/template"

	"github.com/muvaf/typewriter/pkg/wrapper"
	"github.com/pkg/errors"

	"github.com/crossplane/upjet/pkg/config"
	"github.com/crossplane/upjet/pkg/pipeline/templates"
)

// NewProviderGenerator returns a new ProviderGenerator.
func NewProviderGenerator(rootDir, modulePath string) *ProviderGenerator {
	return &ProviderGenerator{
		ProviderPath:       filepath.Join(rootDir, "cmd", "provider"),
		LocalDirectoryPath: filepath.Join(rootDir, "internal", "controller"),
		LicenseHeaderPath:  filepath.Join(rootDir, "hack", "boilerplate.go.txt"),
		ModulePath:         modulePath,
	}
}

// ProviderGenerator generates controller setup file.
type ProviderGenerator struct {
	ProviderPath       string
	LocalDirectoryPath string
	LicenseHeaderPath  string
	ModulePath         string
}

// Generate writes the setup file and the corresponding provider main file
// using the given list of version packages.
func (sg *ProviderGenerator) Generate(versionPkgMap map[string][]string, mainTemplate string) error {
	var t *template.Template
	if len(mainTemplate) != 0 {
		tmpl, err := template.New("main").Parse(mainTemplate)
		if err != nil {
			return errors.Wrap(err, "failed to parse the provider main program template")
		}
		t = tmpl
	}
	if t == nil {
		return errors.Wrap(sg.generate("", versionPkgMap[config.PackageNameMonolith]), "failed to generate the controller setup file")
	}
	for g, versionPkgList := range versionPkgMap {
		if err := sg.generate(g, versionPkgList); err != nil {
			return errors.Wrapf(err, "failed to generate the controller setup file for group: %s", g)
		}
		if err := generateProviderMain(sg.ProviderPath, g, t); err != nil {
			return errors.Wrapf(err, "failed to write main program for group: %s", g)
		}
	}
	return nil
}

func generateProviderMain(providerPath, groupPrefix string, t *template.Template) error {
	f := filepath.Join(providerPath, groupPrefix)
	if err := os.MkdirAll(f, 0750); err != nil {
		return errors.Wrapf(err, "failed to mkdir provider main program path: %s", f)
	}
	m, err := os.OpenFile(filepath.Join(filepath.Clean(f), "zz_main.go"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return errors.Wrap(err, "failed to open provider main program file")
	}
	defer func() {
		if err := m.Close(); err != nil {
			log.Fatalf("Failed to close the templated main %q: %s", f, err.Error())
		}
	}()
	if err := t.Execute(m, map[string]any{
		"GroupPrefix": groupPrefix,
	}); err != nil {
		return errors.Wrap(err, "failed to execute provider main program template")
	}
	return nil
}

func (sg *ProviderGenerator) generate(group string, versionPkgList []string) error {
	setupFile := wrapper.NewFile(filepath.Join(sg.ModulePath, "apis"), "apis", templates.SetupTemplate,
		wrapper.WithGenStatement(GenStatement),
		wrapper.WithHeaderPath(sg.LicenseHeaderPath),
	)
	sort.Strings(versionPkgList)
	aliases := make([]string, len(versionPkgList))
	for i, pkgPath := range versionPkgList {
		aliases[i] = setupFile.Imports.UsePackage(pkgPath)
	}
	g := ""
	if len(group) != 0 {
		g = "_" + group
	}
	vars := map[string]any{
		"Aliases":     aliases,
		"Group":       g,
		"GroupPrefix": group,
	}
	filePath := ""
	if len(group) == 0 {
		filePath = filepath.Join(sg.LocalDirectoryPath, "zz_setup.go")
	} else {
		filePath = filepath.Join(sg.LocalDirectoryPath, fmt.Sprintf("zz_%s_setup.go", group))
	}
	return errors.Wrap(setupFile.Write(filePath, vars, os.ModePerm), "cannot write setup file")
}
