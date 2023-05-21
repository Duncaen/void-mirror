package config

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/zclconf/go-cty/cty"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/dynblock"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/json"
)

type Config struct {
	Repositories []RepositoryConfig `hcl:"repository,block"`
}

type RepositoryConfig struct {
	Upstream string   `hcl:"upstream"`
	Archs    []string `hcl:"archs"`
}

type local struct {
	Name string
	Expr hcl.Expression
}

func decodeLocalsBlock(block *hcl.Block) ([]*local, hcl.Diagnostics) {
	attrs, diags := block.Body.JustAttributes()
	if len(attrs) == 0 {
		return nil, diags
	}
	locals := make([]*local, 0, len(attrs))
	for name, attr := range attrs {
		if !hclsyntax.ValidIdentifier(name) {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid variable name",
				Subject:  &attr.NameRange,
			})
		}
		locals = append(locals, &local{
			Name: name,
			Expr: attr.Expr,
		})
	}
	return locals, diags
}

func (c *Config) Load(filename string) error {
	var file *hcl.File
	var diags hcl.Diagnostics
	src, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	switch suffix := strings.ToLower(filepath.Ext(filename)); suffix {
	case ".hcl":
		file, diags = hclsyntax.ParseConfig(src, filename, hcl.Pos{Line: 1, Column: 1})
	case ".json":
		file, diags = json.Parse(src, filename)
	default:
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Unsupported file format",
			Detail:   fmt.Sprintf("Cannot read from %s: unrecognized file format suffix %q.", filename, suffix),
		})
		return diags
	}
	if diags.HasErrors() {
		return diags
	}
	body := dynblock.Expand(file.Body, nil)

	content, remaining, diags := body.PartialContent(&hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{
				Type: "locals",
			},
		},
	})
	if diags.HasErrors() {
		return diags
	}
	ctx := hcl.EvalContext{Variables: make(map[string]cty.Value)}
	for _, block := range content.Blocks {
		switch block.Type {
		case "locals":
			locals, diags := decodeLocalsBlock(block)
			if diags.HasErrors() {
				return diags
			}
			for _, local := range locals {
				value, diags := local.Expr.Value(&ctx)
				if diags.HasErrors() {
					return diags
				}
				ctx.Variables[local.Name] = value
			}
		}
	}
	diags = gohcl.DecodeBody(remaining, &ctx, c)
	if diags.HasErrors() {
		return diags
	}
	if len(diags) > 0 {
		log.Println(diags)
	}
	return nil
}
