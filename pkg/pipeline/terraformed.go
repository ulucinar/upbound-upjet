// SPDX-FileCopyrightText: 2023 The Crossplane Authors <https://crossplane.io>
//
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"fmt"
	"go/types"
	"os"
	"path/filepath"
	"strings"

	"github.com/muvaf/typewriter/pkg/wrapper"
	"github.com/pkg/errors"

	"github.com/crossplane/upjet/pkg/pipeline/templates"
)

// NewTerraformedGenerator returns a new TerraformedGenerator.
func NewTerraformedGenerator(pkg *types.Package, rootDir, group, version string) *TerraformedGenerator {
	groupPrefix := strings.ToLower(strings.Split(group, ".")[0])
	return &TerraformedGenerator{
		LocalDirectoryPath: filepath.Join(rootDir, "apis", groupPrefix, version),
		LicenseHeaderPath:  filepath.Join(rootDir, "hack", "boilerplate.go.txt"),
		pkg:                pkg,
		groupPrefix:        groupPrefix,
	}
}

// TerraformedGenerator generates conversion methods implementing Terraformed
// interface on CRD structs.
type TerraformedGenerator struct {
	LocalDirectoryPath string
	LicenseHeaderPath  string

	pkg         *types.Package
	groupPrefix string
}

// Generate writes generated Terraformed interface functions
func (tg *TerraformedGenerator) Generate(cfgs []*terraformedInput, apiVersion string) error {
	for _, cfg := range cfgs {
		trFile := wrapper.NewFile(tg.pkg.Path(), tg.pkg.Name(), templates.TerraformedTemplate,
			wrapper.WithGenStatement(GenStatement),
			wrapper.WithHeaderPath(tg.LicenseHeaderPath),
		)
		filePath := filepath.Join(tg.LocalDirectoryPath, fmt.Sprintf("zz_%s_terraformed.go", strings.ToLower(cfg.Kind)))

		vars := map[string]any{
			"APIVersion": apiVersion,
		}
		vars["CRD"] = map[string]string{
			"Kind":               cfg.Kind,
			"GroupPrefix":        tg.groupPrefix,
			"ParametersTypeName": cfg.ParametersTypeName,
		}
		vars["Terraform"] = map[string]any{
			"ResourceType":  cfg.Name,
			"SchemaVersion": cfg.TerraformResource.SchemaVersion,
		}
		vars["Sensitive"] = map[string]any{
			"Fields": cfg.Sensitive.GetFieldPaths(),
		}
		vars["LateInitializer"] = map[string]any{
			"IgnoredFields": cfg.LateInitializer.GetIgnoredCanonicalFields(),
		}

		if err := trFile.Write(filePath, vars, os.ModePerm); err != nil {
			return errors.Wrapf(err, "cannot write the Terraformed interface implementation file %s", filePath)
		}
	}
	return nil
}
