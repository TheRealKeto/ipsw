/*
Copyright © 2018-2022 blacktop

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
*/
package cmd

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/gob"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"text/tabwriter"

	"github.com/apex/log"
	"github.com/blacktop/go-macho"
	"github.com/blacktop/go-plist"
	"github.com/blacktop/ipsw/internal/utils"
	"github.com/blacktop/ipsw/pkg/info"
	"github.com/spf13/cobra"
)

type Entitlements map[string]interface{}

func scanEnts(ipswPath, dmgPath, dmgType string) (map[string]string, error) {
	if utils.StrSliceHas(haveChecked, dmgPath) {
		return nil, nil // already checked
	}

	dmgs, err := utils.Unzip(ipswPath, "", func(f *zip.File) bool {
		return strings.EqualFold(filepath.Base(f.Name), dmgPath)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to extract %s from IPSW: %v", dmgPath, err)
	}
	if len(dmgs) == 0 {
		return nil, fmt.Errorf("failed to find %s in IPSW", dmgPath)
	}
	defer os.Remove(dmgs[0])

	utils.Indent(log.Info, 3)(fmt.Sprintf("Mounting %s %s", dmgType, dmgs[0]))
	mountPoint, err := utils.MountFS(dmgs[0])
	if err != nil {
		return nil, fmt.Errorf("failed to mount DMG: %v", err)
	}
	defer func() {
		utils.Indent(log.Info, 3)(fmt.Sprintf("Unmounting %s", dmgs[0]))
		if err := utils.Unmount(mountPoint, false); err != nil {
			log.Errorf("failed to unmount DMG at %s: %v", dmgs[0], err)
		}
	}()

	var files []string
	if err := filepath.Walk(mountPoint, func(path string, info os.FileInfo, err error) error {
		if !info.IsDir() {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("failed to walk files in dir %s: %v", mountPoint, err)
	}

	entDB := make(map[string]string)

	for _, file := range files {
		if m, err := macho.Open(file); err == nil {
			if m.CodeSignature() != nil && len(m.CodeSignature().Entitlements) > 0 {
				entDB[strings.TrimPrefix(file, mountPoint)] = m.CodeSignature().Entitlements
			} else {
				entDB[strings.TrimPrefix(file, mountPoint)] = ""
			}
		}
	}

	haveChecked = append(haveChecked, dmgPath)

	return entDB, nil
}

func init() {
	rootCmd.AddCommand(entCmd)

	entCmd.Flags().StringP("ent", "e", "", "Entitlement to search for")
	entCmd.Flags().String("db", "", "Path to entitlement database to use")
	entCmd.Flags().StringP("file", "f", "", "Output entitlements for file")
}

// entCmd represents the ent command
var entCmd = &cobra.Command{
	Use:          "ent <IPSW>",
	Short:        "Search IPSW filesystem DMG for MachOs with a given entitlement",
	Args:         cobra.MinimumNArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {

		if Verbose {
			log.SetLevel(log.DebugLevel)
		}

		entitlement, _ := cmd.Flags().GetString("ent")
		entDBPath, _ := cmd.Flags().GetString("db")
		searchFile, _ := cmd.Flags().GetString("file")

		if len(entitlement) == 0 && len(searchFile) == 0 {
			log.Errorf("you must supply a --ent OR --file")
			return nil
		} else if len(entitlement) > 0 && len(searchFile) > 0 {
			log.Errorf("you can only use --ent OR --file (not both)")
			return nil
		}

		ipswPath := filepath.Clean(args[0])

		if len(entDBPath) == 0 {
			entDBPath = strings.TrimSuffix(ipswPath, filepath.Ext(ipswPath)) + ".entDB"
		}

		i, err := info.Parse(ipswPath)
		if err != nil {
			return fmt.Errorf("failed to parse IPSW: %v", err)
		}

		entDB := make(map[string]string)

		if _, err := os.Stat(entDBPath); os.IsNotExist(err) {
			log.Info("Generating entitlement database file...")

			if appOS, err := i.GetAppOsDmg(); err == nil {
				if ents, err := scanEnts(ipswPath, appOS, "AppOS"); err != nil {
					return fmt.Errorf("failed to scan files in AppOS %s: %v", appOS, err)
				} else {
					for k, v := range ents {
						entDB[k] = v
					}
				}
			}
			if systemOS, err := i.GetSystemOsDmg(); err == nil {
				if ents, err := scanEnts(ipswPath, systemOS, "SystemOS"); err != nil {
					return fmt.Errorf("failed to scan files in SystemOS %s: %v", systemOS, err)
				} else {
					for k, v := range ents {
						entDB[k] = v
					}
				}
			}
			if fsOS, err := i.GetFileSystemOsDmg(); err == nil {
				if ents, err := scanEnts(ipswPath, fsOS, "filesystem"); err != nil {
					return fmt.Errorf("failed to scan files in filesystem %s: %v", fsOS, err)
				} else {
					for k, v := range ents {
						entDB[k] = v
					}
				}
			}

			buff := new(bytes.Buffer)

			e := gob.NewEncoder(buff)

			// Encoding the map
			err := e.Encode(entDB)
			if err != nil {
				return fmt.Errorf("failed to encode entitlement db to binary: %v", err)
			}

			of, err := os.Create(entDBPath)
			if err != nil {
				return fmt.Errorf("failed to create file %s: %v", ipswPath+".entDB", err)
			}
			defer of.Close()

			gzw := gzip.NewWriter(of)
			defer gzw.Close()

			_, err = buff.WriteTo(gzw)
			if err != nil {
				return fmt.Errorf("failed to write entitlement db to gzip file: %v", err)
			}
		} else {
			log.Info("Found ipsw entitlement database file...")

			edbFile, err := os.Open(entDBPath)
			if err != nil {
				return fmt.Errorf("failed to open entitlement database file %s; %v", entDBPath, err)
			}

			gzr, err := gzip.NewReader(edbFile)
			if err != nil {
				return fmt.Errorf("failed to create gzip reader: %v", err)
			}

			// Decoding the serialized data
			err = gob.NewDecoder(gzr).Decode(&entDB)
			if err != nil {
				return fmt.Errorf("failed to decode entitlement database; %v", err)
			}
			gzr.Close()
			edbFile.Close()
		}

		if len(searchFile) > 0 {
			for f, ent := range entDB {
				if strings.Contains(strings.ToLower(f), strings.ToLower(searchFile)) {
					log.Infof(f)
					if len(ent) > 0 {
						fmt.Printf("\n%s\n", ent)
					} else {
						fmt.Printf("\n\t- no entitlements\n")
					}
				}
			}
		} else {
			log.Infof("Files containing entitlement: %s", entitlement)
			fmt.Println()
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', 0)
			for f, ent := range entDB {
				if strings.Contains(ent, entitlement) {
					ents := Entitlements{}
					if err := plist.NewDecoder(bytes.NewReader([]byte(ent))).Decode(&ents); err != nil {
						return fmt.Errorf("failed to decode entitlements plist for %s: %v", f, err)
					}
					for k, v := range ents {
						if strings.Contains(k, entitlement) {
							switch v := reflect.ValueOf(v); v.Kind() {
							case reflect.Bool:
								if v.Bool() {
									fmt.Fprintf(w, "%s\t%s\n", k, f)
								}
							default:
								log.Error(fmt.Sprintf("unhandled entitlement kind %s in %s", f, v.Kind()))
							}
						}
					}
				}
			}
			w.Flush()
		}

		return nil
	},
}
