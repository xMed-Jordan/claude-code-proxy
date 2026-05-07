package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type buildTarget struct {
	GOOS   string
	GOARCH string
}

var allBuildTargets = []buildTarget{
	{"windows", "amd64"},
	{"windows", "arm64"},
	{"linux", "amd64"},
	{"linux", "arm64"},
	{"darwin", "amd64"},
	{"darwin", "arm64"},
}

func runBuild(args []string) error {
	all := false
	for _, arg := range args {
		if arg == "--all" {
			all = true
		}
	}
	if all {
		for _, target := range allBuildTargets {
			if err := buildTargetBinary(target); err != nil {
				return err
			}
		}
		return nil
	}
	return ensureCurrentBinaryBuilt()
}

func buildTargetBinary(target buildTarget) error {
	name := proxyBinaryNameFor(target.GOOS)
	outDir := filepath.Join(mustGetwd(), "bin", target.GOOS+"-"+target.GOARCH)
	if err := os.MkdirAll(outDir, 0700); err != nil {
		return err
	}
	out := filepath.Join(outDir, name)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, ".")
	cmd.Dir = mustGetwd()
	cmd.Env = append(os.Environ(), "GOOS="+target.GOOS, "GOARCH="+target.GOARCH)
	raw, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("build %s failed: %s", formatPlatformTarget(target.GOOS, target.GOARCH), strings.TrimSpace(string(raw)))
	}
	_ = os.Chmod(out, executableModeFor(target.GOOS))
	fmt.Printf("Built %s -> %s\n", formatPlatformTarget(target.GOOS, target.GOARCH), out)
	return nil
}

func runPackageExtension() error {
	if err := ensureCurrentBinaryBuilt(); err != nil {
		return err
	}
	distRoot := filepath.Join(mustGetwd(), "dist")
	extensionRoot := filepath.Join(distRoot, "connect-ai-proxy-browser")
	serverBinRoot := filepath.Join(extensionRoot, "server", "bin")
	if err := os.RemoveAll(extensionRoot); err != nil {
		return err
	}
	if err := os.MkdirAll(serverBinRoot, 0700); err != nil {
		return err
	}
	binSource := currentProxyBinaryPath()
	binDest := filepath.Join(serverBinRoot, currentProxyBinaryName())
	if err := copyFile(binSource, binDest, executableModeFor(runtime.GOOS)); err != nil {
		return err
	}
	manifest := desktopExtensionManifest()
	rawManifest, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	rawManifest = append(rawManifest, '\n')
	if err := os.WriteFile(filepath.Join(extensionRoot, "manifest.json"), rawManifest, 0600); err != nil {
		return err
	}
	zipPath := filepath.Join(distRoot, "connect-ai-proxy-browser.zip")
	mcpbPath := filepath.Join(distRoot, "connect-ai-proxy-browser.mcpb")
	dxtPath := filepath.Join(distRoot, "connect-ai-proxy-browser.dxt")
	for _, artifact := range []string{zipPath, mcpbPath, dxtPath} {
		_ = os.Remove(artifact)
	}
	if err := zipDirectory(extensionRoot, zipPath); err != nil {
		return err
	}
	if err := copyFile(zipPath, mcpbPath, 0600); err != nil {
		return err
	}
	if err := copyFile(zipPath, dxtPath, 0600); err != nil {
		return err
	}
	out := map[string]string{
		"unpacked_extension": extensionRoot,
		"zip_package":        zipPath,
		"mcpb_package":       mcpbPath,
		"dxt_package":        dxtPath,
		"manifest":           filepath.Join(extensionRoot, "manifest.json"),
	}
	raw, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(raw))
	return nil
}

func desktopExtensionManifest() map[string]any {
	platform := map[string]string{"windows": "win32", "darwin": "darwin", "linux": "linux"}[runtime.GOOS]
	if platform == "" {
		platform = runtime.GOOS
	}
	binary := "server/bin/" + currentProxyBinaryName()
	return map[string]any{
		"manifest_version": "0.3",
		"name":             "connect-ai-proxy-browser",
		"display_name":     "Connect AI Proxy Browser",
		"version":          "0.1.0",
		"description":      "Local visible browser-control MCP bridge for Claude Desktop and Claude Code.",
		"long_description": "Connect AI Proxy Browser exposes local browser-control tools through MCP. It can check browser status, list pages, navigate, read visible page text, capture screenshots, inspect console/network errors, move a visible cursor overlay, click, type, press keys, and wait for page content.",
		"author":           map[string]string{"name": "Local Connect AI Proxy"},
		"license":          "MIT",
		"keywords":         []string{"browser", "mcp", "automation", "local", "connect-ai-proxy"},
		"tools": []map[string]string{
			{"name": "browser_status", "description": "Check Chrome, Antigravity extension, and browser-control connection status."},
			{"name": "browser_pages", "description": "List open Chrome pages available to the browser bridge."},
			{"name": "browser_navigate", "description": "Open or navigate the active Chrome page to a URL."},
			{"name": "browser_snapshot", "description": "Read visible page text and a short list of interactive elements."},
			{"name": "browser_screenshot", "description": "Take a screenshot of the active Chrome page and save it to a local PNG file."},
			{"name": "browser_console", "description": "Check the active Chrome page for console errors, runtime exceptions, failed loads, and HTTP errors."},
			{"name": "browser_move", "description": "Move the visible overlay cursor to a selector, text, or coordinate without clicking."},
			{"name": "browser_click", "description": "Move the visible overlay cursor and click a selector, text, or coordinate."},
			{"name": "browser_type", "description": "Click a target, optionally clear it, and type text into the focused field."},
			{"name": "browser_press_key", "description": "Press a keyboard key in the active page, such as Enter, Tab, Escape, or Backspace."},
			{"name": "browser_wait", "description": "Wait until a selector or visible text appears in the active page."},
		},
		"server": map[string]any{
			"type":        "binary",
			"entry_point": binary,
			"mcp_config": map[string]any{
				"command": "${__dirname}/" + binary,
				"args":    []string{"browser-mcp"},
				"env":     map[string]string{"CCP_BROWSER_MCP": "antigravity-browser"},
			},
		},
		"compatibility": map[string]any{
			"claude_desktop": ">=0.10.0",
			"platforms":      []string{platform},
		},
	}
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

func zipDirectory(root, dst string) error {
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	defer zw.Close()
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = rel
		if runtime.GOOS != "windows" && strings.Contains(rel, "server/bin/") {
			header.SetMode(0700)
		}
		writer, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(writer, in)
		return err
	})
}
