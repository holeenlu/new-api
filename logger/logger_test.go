package logger

import (
	"os"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetupLoggerKeepsCapturedGinWriterLiveAcrossRotation(t *testing.T) {
	originalLogDir := *common.LogDir
	common.LogWriterMu.Lock()
	originalDefaultWriter := gin.DefaultWriter
	originalDefaultErrorWriter := gin.DefaultErrorWriter
	currentLogPathMu.Lock()
	originalPath := currentLogPath
	originalFile := currentLogFile
	currentLogPath = ""
	currentLogFile = nil
	currentLogPathMu.Unlock()
	common.LogWriterMu.Unlock()

	*common.LogDir = t.TempDir()
	t.Cleanup(func() {
		*common.LogDir = originalLogDir
		common.LogWriterMu.Lock()
		accessLogWriter.setTarget(os.Stdout)
		errorLogWriter.setTarget(os.Stderr)
		gin.DefaultWriter = originalDefaultWriter
		gin.DefaultErrorWriter = originalDefaultErrorWriter
		currentLogPathMu.Lock()
		rotatedFile := currentLogFile
		currentLogPath = originalPath
		currentLogFile = originalFile
		currentLogPathMu.Unlock()
		if rotatedFile != nil {
			_ = rotatedFile.Close()
		}
		common.LogWriterMu.Unlock()
	})

	SetupLogger()
	capturedWriter := gin.DefaultWriter
	firstFile := currentLogFile
	require.Same(t, accessLogWriter, capturedWriter)
	require.NotNil(t, firstFile)
	_, err := capturedWriter.Write([]byte("before rotation\n"))
	require.NoError(t, err)

	SetupLogger()
	assert.Same(t, capturedWriter, gin.DefaultWriter)
	assert.NotSame(t, firstFile, currentLogFile)
	_, err = capturedWriter.Write([]byte("after rotation\n"))
	require.NoError(t, err, "Gin keeps the writer captured when middleware is installed")

	content, err := os.ReadFile(GetCurrentLogPath())
	require.NoError(t, err)
	assert.Contains(t, string(content), "after rotation")
}
