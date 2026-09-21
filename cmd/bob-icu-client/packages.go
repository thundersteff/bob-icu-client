package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
)

//go:embed packages/*.json
var packageFiles embed.FS

type standardPackage struct {
	PackageID   string          `json:"package_id"`
	Description string          `json:"description"`
	Services    []ServiceConfig `json:"services"`
}

func loadPackages() (map[string]standardPackage, error) {
	names, err := fs.Glob(packageFiles, "packages/*.json")
	if err != nil {
		return nil, err
	}
	result := make(map[string]standardPackage, len(names))
	for _, name := range names {
		data, err := packageFiles.ReadFile(name)
		if err != nil {
			return nil, err
		}
		var pkg standardPackage
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&pkg); err != nil {
			return nil, fmt.Errorf("decode embedded package %s: %w", name, err)
		}
		if decoder.Decode(&struct{}{}) != io.EOF {
			return nil, fmt.Errorf("decode embedded package %s: trailing data", name)
		}
		if !keyRE.MatchString(pkg.PackageID) || strings.TrimSpace(pkg.Description) == "" || len(pkg.Services) == 0 {
			return nil, fmt.Errorf("invalid embedded package %s", name)
		}
		if _, exists := result[pkg.PackageID]; exists {
			return nil, fmt.Errorf("duplicate embedded package %s", pkg.PackageID)
		}
		result[pkg.PackageID] = pkg
	}
	return result, nil
}

func expandPackages(cfg *Config) error {
	available, err := loadPackages()
	if err != nil {
		return err
	}
	seenPackages := make(map[string]bool, len(cfg.Packages))
	seenServices := make(map[string]bool, len(cfg.Services))
	for _, svc := range cfg.Services {
		seenServices[svc.ServiceID] = true
	}
	var expanded []ServiceConfig
	for _, packageID := range cfg.Packages {
		if seenPackages[packageID] {
			return fmt.Errorf("duplicate package %q", packageID)
		}
		seenPackages[packageID] = true
		pkg, ok := available[packageID]
		if !ok {
			return fmt.Errorf("unknown package %q", packageID)
		}
		for _, svc := range pkg.Services {
			if seenServices[svc.ServiceID] {
				return fmt.Errorf("package %q conflicts with service %q", packageID, svc.ServiceID)
			}
			seenServices[svc.ServiceID] = true
			expanded = append(expanded, svc)
		}
	}
	cfg.Services = append(expanded, cfg.Services...)
	return nil
}

func printPackages(w io.Writer) error {
	packages, err := loadPackages()
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(packages))
	for id := range packages {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		pkg := packages[id]
		if _, err := fmt.Fprintf(w, "%s\t%s\n", pkg.PackageID, pkg.Description); err != nil {
			return err
		}
	}
	return nil
}
