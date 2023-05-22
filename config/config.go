package config

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
	"github.com/zclconf/go-cty/cty/function/stdlib"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/dynblock"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/json"
)

type Config struct {
	Repositories []*RepositoryConfig `hcl:"repository,block"`
	Jobs         int                 `hcl:"jobs,optional"`
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

type RepositoryConfig struct {
	Upstream     *url.URL
	Destination  string
	Architecture string
	Interval     *time.Duration
}

func decodeRepositoryBlock(block *hcl.Block, ctx *hcl.EvalContext) (*RepositoryConfig, hcl.Diagnostics) {
	var data struct {
		Upstream     string `hcl:"upstream"`
		Destination  string `hcl:"destination"`
		Architecture string `hcl:"architecture"`
		Interval     string `hcl:"interval,optional"`
	}
	diags := gohcl.DecodeBody(block.Body, ctx, &data)
	if diags.HasErrors() {
		return nil, diags
	}
	repo := &RepositoryConfig{
		Destination:  data.Destination,
		Architecture: data.Architecture,
	}
	var err error
	repo.Upstream, err = url.Parse(data.Upstream)
	if err != nil {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid upstream",
			Detail:   fmt.Sprintf("Invalid upstream: %q: %v", data.Upstream, err),
		})
		return nil, diags
	}
	if data.Interval != "" {
		interval, err := time.ParseDuration(data.Interval)
		if err != nil {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid interval",
				Detail:   fmt.Sprintf("Invalid interval: %q: %v", data.Interval, err),
			})
			return nil, diags
		}
		repo.Interval = &interval
	}
	return repo, diags
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
	ctx := hcl.EvalContext{
		Variables: make(map[string]cty.Value),
		Functions: map[string]function.Function{
			"concat":  stdlib.ConcatFunc,
			"flatten": stdlib.FlattenFunc,
			"merge":   stdlib.MergeFunc,
		},
	}
	body := dynblock.Expand(file.Body, &ctx)

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
	content, diags = remaining.Content(&hcl.BodySchema{
		Attributes: []hcl.AttributeSchema{
			{
				Name: "jobs",
			},
		},
		Blocks: []hcl.BlockHeaderSchema{
			{
				Type: "repository",
			},
		},
	})
	if diags.HasErrors() {
		return diags
	}

	for _, block := range content.Blocks {
		switch block.Type {
		case "repository":
			repo, diags := decodeRepositoryBlock(block, &ctx)
			if diags.HasErrors() {
				return diags
			}
			c.Repositories = append(c.Repositories, repo)
		}
	}
	for name, attr := range content.Attributes {
		switch name {
		case "jobs":
			diags := gohcl.DecodeExpression(attr.Expr, &ctx, &c.Jobs)
			if diags.HasErrors() {
				return diags
			}
		}

	}

	if len(diags) > 0 {
		log.Println(diags)
	}
	return nil
}
