package ui

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bluekeyes/go-gitdiff/gitdiff"

	"github.com/dlvhdr/diffnav/pkg/constants"
	"github.com/dlvhdr/diffnav/pkg/filenode"
)

func sortFiles(files []*gitdiff.File) {
	slices.SortFunc(files, func(a *gitdiff.File, b *gitdiff.File) int {
		nameA := filenode.GetFileName(a)
		nameB := filenode.GetFileName(b)
		dira := filepath.Dir(nameA)
		dirb := filepath.Dir(nameB)
		if dira != constants.RootName && dirb != constants.RootName && dira == dirb {
			return strings.Compare(strings.ToLower(nameA), strings.ToLower(nameB))
		}

		if dira != constants.RootName && dirb == constants.RootName {
			return -1
		}
		if dirb != constants.RootName && dira == constants.RootName {
			return 1
		}

		if dira != constants.RootName && dirb != constants.RootName {
			if strings.HasPrefix(dira, dirb) {
				return -1
			}

			if strings.HasPrefix(dirb, dira) {
				return 1
			}
		}

		return strings.Compare(strings.ToLower(nameA), strings.ToLower(nameB))
	})
}

// diffFileSets compares old and new file lists and returns which files changed, were added, or removed.
func diffFileSets(oldFiles, newFiles []*gitdiff.File) (changed, added, removed []string) {
	oldMap := make(map[string]string, len(oldFiles))
	for _, f := range oldFiles {
		oldMap[filenode.GetFileName(f)] = f.String()
	}
	for _, f := range newFiles {
		name := filenode.GetFileName(f)
		if oldDiff, ok := oldMap[name]; ok {
			if f.String() != oldDiff {
				changed = append(changed, name)
			}
			delete(oldMap, name)
		} else {
			added = append(added, name)
		}
	}
	for name := range oldMap {
		removed = append(removed, name)
	}
	return
}

func relativeTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		m := int(d.Minutes())
		return fmt.Sprintf("%dm", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		return fmt.Sprintf("%dh", h)
	case d < 30*24*time.Hour:
		days := int(d.Hours() / 24)
		return fmt.Sprintf("%dd", days)
	case d < 365*24*time.Hour:
		months := int(d.Hours() / 24 / 30)
		return fmt.Sprintf("%dmo", months)
	default:
		years := int(d.Hours() / 24 / 365)
		return fmt.Sprintf("%dy", years)
	}
}
