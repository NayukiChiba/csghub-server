package workflows

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"go.temporal.io/sdk/activity"
	"opencsg.com/csghub-server/common/types"
	dvCom "opencsg.com/csghub-server/dataviewer/common"
)

func TransferFileObject(file *dvCom.RepoFile, subsetName, splitName string) dvCom.FileObject {
	return dvCom.FileObject{
		RepoFile:        file.Path,
		Size:            file.Size,
		LastCommit:      file.LastCommitSHA,
		Lfs:             file.Lfs,
		LfsRelativePath: file.LfsRelativePath,
		SubsetName:      subsetName,
		SplitName:       splitName,
		DownloadSize:    file.DownloadSize,
		LfsSHA256:       file.LfsSHA256,
	}
}

func GetPatternFileList(path interface{}) []string {
	if path == nil {
		return []string{}
	}
	var (
		files []string
	)
	if slice, ok := path.([]interface{}); ok {
		for _, v := range slice {
			files = append(files, v.(string))
		}
	} else if slice, ok := path.([]string); ok {
		files = slice
	} else {
		files = []string{path.(string)}
	}
	return files
}

func ConvertRealFiles(splitFiles []string, filePaths []string, targetFiles map[string]*dvCom.RepoFile, subsetName, splitName string) []dvCom.FileObject {
	var phyFiles []dvCom.FileObject
	for _, filePattern := range splitFiles {
		if !strings.Contains(filePattern, dvCom.WILDCARD) || !doublestar.ValidatePathPattern(filePattern) {
			file, exists := targetFiles[filePattern]
			if exists {
				phyFiles = append(phyFiles, TransferFileObject(file, subsetName, splitName))
			}
			continue
		}

		for _, path := range filePaths {
			match, err := doublestar.PathMatch(filePattern, path)
			if err != nil {
				slog.Error("file pattern match", "error", err)
			}
			if match {
				file, exists := targetFiles[path]
				if exists {
					phyFiles = append(phyFiles, TransferFileObject(file, subsetName, splitName))
				}
			}
		}
	}
	slog.Debug("convert real files", slog.Any("splitFiles", splitFiles), slog.Any("phyFiles", phyFiles))
	return phyFiles
}

func GetCardDataMD5(finalCardData dvCom.CardData) string {
	hasher := md5.New()

	for _, subset := range finalCardData.Configs {
		fmt.Fprintf(hasher, "%s", subset.ConfigName)
		for _, split := range subset.DataFiles {
			fmt.Fprintf(hasher, "%s", split.Split)
		}
	}

	for _, info := range finalCardData.DatasetInfos {
		for _, split := range info.Splits {
			for _, file := range split.Origins {
				fmt.Fprintf(hasher, "%s-%s", file.RepoFile, file.LastCommit)
			}
		}
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func GenerateWorkFlowID(req types.UpdateViewerReq) string {
	id := "dv-" + req.Namespace + "/" + req.Name + "-" + time.Now().Format("20060102.150405.000000000")
	return id
}

func GetCacheRepoPath(ctx context.Context, cacheDir string, req types.UpdateViewerReq) string {
	wfCtx := activity.GetInfo(ctx)
	workflowID := wfCtx.WorkflowExecution.ID
	segments := strings.Split(workflowID, "-")
	timeSeq := segments[len(segments)-1]
	return fmt.Sprintf("%s/%s/%s/%s/%s", strings.TrimSuffix(cacheDir, "/"), req.RepoType, req.Namespace, req.Name, timeSeq)
}

func CopyJsonArray(writeFile *os.File, reader io.ReadCloser, limitSize int64) (int64, error) {
	decoder := json.NewDecoder(withoutUTF8BOM(reader))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return 0, fmt.Errorf("invalid json array object error: %w", err)
	}
	writer := NewFileWriter(writeFile)
	encoder := json.NewEncoder(writer)
	_, err = writer.Write([]byte{'['})
	if err != nil {
		return 0, fmt.Errorf("write json begin delimiter error: %w", err)
	}
	count := 0
	var copyedBytes int64
	for decoder.More() {
		if copyedBytes >= limitSize {
			break
		}
		var obj map[string]interface{}
		if err := decoder.Decode(&obj); err != nil {
			return 0, fmt.Errorf("parse json object error: %w", err)
		}
		if count > 0 {
			_, err = writer.Write([]byte{','})
			if err != nil {
				return 0, fmt.Errorf("write comma error: %w", err)
			}
		}

		err = encoder.Encode(obj)
		if err != nil {
			return 0, fmt.Errorf("encode json object error: %w", err)
		}
		count++
		copyedBytes += int64(writer.GetWriteBytes())
	}
	_, err = writer.Write([]byte{']'})
	if err != nil {
		return 0, fmt.Errorf("write json end delimiter error: %w", err)
	}
	return copyedBytes, nil
}

// withoutUTF8BOM accepts UTF-8 BOM-prefixed JSON without altering sample content.
func withoutUTF8BOM(reader io.Reader) *bufio.Reader {
	buffered := bufio.NewReader(reader)
	if prefix, _ := buffered.Peek(3); bytes.Equal(prefix, []byte{0xef, 0xbb, 0xbf}) {
		_, _ = buffered.Discard(3)
	}
	return buffered
}

func CopyFileContext(writeFile *os.File, reader io.ReadCloser, limitSize int64) (int64, error) {
	buffered := withoutUTF8BOM(reader)
	var copied int64
	for copied < limitSize {
		var lineBytes int
		// ReadSlice streams arbitrarily long lines without Scanner's 64 KiB cap.
		// Finish the current record before applying the preview byte budget.
		for {
			data, err := buffered.ReadSlice('\n')
			n, writeErr := writeFile.Write(data)
			copied += int64(n)
			lineBytes += n
			if writeErr != nil {
				return copied, writeErr
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if errors.Is(err, io.EOF) {
				if lineBytes > 0 {
					n, e := writeFile.Write([]byte{'\n'})
					copied += int64(n)
					if e != nil {
						return copied, e
					}
				}
				return copied, nil
			}
			if err != nil {
				return copied, fmt.Errorf("read dataset content: %w", err)
			}
			break
		}
	}
	return copied, nil
}

func GetThreadNum(totalDataSize int64, maxThreadNum int) int {
	threadNum := int(totalDataSize / DataSizePerThread)
	if threadNum < 1 {
		threadNum = 1
	} else if threadNum > maxThreadNum {
		threadNum = maxThreadNum
	}
	return threadNum
}
