package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"
)

type folderMap struct {
	Version        int              `json:"version"`
	AnchorFolderID string           `json:"anchor_folder_id"`
	Folders        []folderMapEntry `json:"folders"`
}

type folderMapEntry struct {
	FolderID    string `json:"folder_id"`
	Reference   string `json:"reference,omitempty"`
	DisplayName string `json:"display_name"`
	RootPath    string `json:"root_path"`
	StatePath   string `json:"state_path"`
}

func writeFolderMap(shared sharedConfig, roots []syncRoot, configs []config) error {
	if len(roots) != len(configs) {
		return errors.New("folder map input lengths do not match")
	}
	value := folderMap{
		Version:        1,
		AnchorFolderID: shared.baseFolderID,
		Folders:        make([]folderMapEntry, 0, len(roots)),
	}
	for index, root := range roots {
		value.Folders = append(value.Folders, folderMapEntry{
			FolderID:    configs[index].folderID,
			Reference:   root.Reference,
			DisplayName: rootDisplayName(root),
			RootPath:    root.Path,
			StatePath:   configs[index].statePath,
		})
	}
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode folder map: %w", err)
	}
	content = append(content, '\n')

	path, err := folderMapPath(shared)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create folder map directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open folder map: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return fmt.Errorf("protect folder map: %w", err)
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		return fmt.Errorf("write folder map: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync folder map: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close folder map: %w", err)
	}
	return nil
}

func folderMapPath(shared sharedConfig) (string, error) {
	path := os.Getenv("SYNC_FOLDER_MAP_PATH")
	if path == "" {
		path = filepath.Join(
			shared.stateDirectory,
			fmt.Sprintf("folders-%s.json", shared.baseFolderID),
		)
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve folder map path: %w", err)
	}
	return path, nil
}

func runFoldersCommand(arguments []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("folders", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	jsonOutput := flags.Bool("json", false, "write the local folder map as JSON")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("folders accepts flags only")
	}
	shared, err := loadSharedConfig()
	if err != nil {
		return err
	}
	path, err := folderMapPath(shared)
	if err != nil {
		return err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read folder map: %w", err)
	}
	var value folderMap
	if err := json.Unmarshal(content, &value); err != nil {
		return fmt.Errorf("decode folder map: %w", err)
	}
	if value.Version != 1 || value.AnchorFolderID != shared.baseFolderID {
		return errors.New("folder map is incompatible with the current configuration")
	}
	if *jsonOutput {
		_, err := output.Write(content)
		return err
	}

	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(writer, "FOLDER_ID\tDISPLAY_NAME\tREFERENCE\tROOT_PATH"); err != nil {
		return err
	}
	for _, entry := range value.Folders {
		if _, err := fmt.Fprintf(
			writer,
			"%s\t%s\t%s\t%s\n",
			entry.FolderID,
			entry.DisplayName,
			entry.Reference,
			entry.RootPath,
		); err != nil {
			return err
		}
	}
	return writer.Flush()
}
