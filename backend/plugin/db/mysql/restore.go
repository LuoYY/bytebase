package mysql

// This file implements recovery functions for MySQL.
// For example, the original database is `dbfoo`. The suffixTs, derived from the PITR issue's CreateTs, is 1653018005.
// Bytebase will do the following:
// 1. Create a database called `dbfoo_pitr_1653018005`, and do PITR restore to it.
// 2. Create a database called `dbfoo_pitr_1653018005_del`, and move tables
// 	  from `dbfoo` to `dbfoo_pitr_1653018005_del`, and tables from `dbfoo_pitr_1653018005` to `dbfoo`.

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/bytebase/bytebase/backend/common"
	"github.com/bytebase/bytebase/backend/common/log"
	api "github.com/bytebase/bytebase/backend/legacyapi"
	"github.com/bytebase/bytebase/backend/plugin/db/util"
	bbs3 "github.com/bytebase/bytebase/backend/plugin/storage/s3"
	"github.com/bytebase/bytebase/backend/resources/mysqlutil"
	storepb "github.com/bytebase/bytebase/proto/generated-go/store"
)

const (
	// Variable lower_case_table_names related.

	// LetterCaseOnDiskLetterCaseCmp stores table and database names using the letter case specified in the CREATE TABLE or CREATE DATABASE statement.
	// Name comparisons are case-sensitive.
	LetterCaseOnDiskLetterCaseCmp = 0
	// LowerCaseOnDiskLowerCaseCmp stores table names in lowercase on disk and name comparisons are not case-sensitive.
	LowerCaseOnDiskLowerCaseCmp = 1
	// LetterCaseOnDiskLowerCaseCmp stores table and database names are stored on disk using the letter case specified in the CREATE TABLE or CREATE DATABASE statement, but MySQL converts them to lowercase on lookup.
	// Name comparisons are not case-sensitive.
	LetterCaseOnDiskLowerCaseCmp = 2

	// binlog metadata file suffix.
	binlogMetaSuffix = ".meta"
)

// ErrParseBinlogName is returned if we failed to parse binlog name.
type ErrParseBinlogName struct {
	err error
}

// IsErrParseBinlogName checks if the underlying error is ErrParseBinlogName.
func IsErrParseBinlogName(err error) bool {
	_, ok := errors.Cause(err).(ErrParseBinlogName)
	return ok
}

func (err ErrParseBinlogName) Error() string {
	return fmt.Sprintf("failed to parse binlog file name: %v", err.err)
}

// BinlogFile is the metadata of the MySQL binlog file.
type BinlogFile struct {
	Name string
	Size int64

	// Seq is parsed from Name and is for the sorting purpose.
	Seq int64
}

func newBinlogFile(name string, size int64) (BinlogFile, error) {
	_, seq, err := ParseBinlogName(name)
	if err != nil {
		return BinlogFile{}, err
	}
	return BinlogFile{Name: name, Size: size, Seq: seq}, nil
}

type binlogFileMeta struct {
	FirstEventTs int64 `json:"first_event_ts"`

	// Do not persist the following fields.
	seq        int64
	binlogName string
}

func readBinlogMetaFile(binlogDir, fileName string) (binlogFileMeta, error) {
	metaFilePath := filepath.Join(binlogDir, fileName)
	fileContent, err := os.ReadFile(metaFilePath)
	if err != nil {
		return binlogFileMeta{}, errors.Wrapf(err, "failed to read binlog metadata file %q", metaFilePath)
	}
	var meta binlogFileMeta
	if err := json.Unmarshal(fileContent, &meta); err != nil {
		return binlogFileMeta{}, errors.Wrapf(err, "failed to unmarshal binlog metadata file %q", metaFilePath)
	}
	binlogFileName := strings.TrimSuffix(fileName, binlogMetaSuffix)
	meta.binlogName = binlogFileName
	_, seq, err := ParseBinlogName(binlogFileName)
	if err != nil {
		return binlogFileMeta{}, errors.Wrapf(err, "failed to get seq from binlog metadata file name %q", fileName)
	}
	meta.seq = seq
	return meta, nil
}

// replayBinlogFromDir replays the binlog for `originDatabase` from `startBinlogInfo.Position` to `targetTs`, read binlog from `binlogDir`.
func (driver *Driver) replayBinlogFromDir(ctx context.Context, originalDatabase, targetDatabase string, startBinlogInfo, targetBinlogInfo api.BinlogInfo, targetTs int64, binlogDir string) error {
	replayBinlogPaths, err := GetBinlogReplayList(startBinlogInfo, targetBinlogInfo, binlogDir)
	if err != nil {
		return errors.Wrapf(err, "failed to get binlog replay list in directory %s", binlogDir)
	}

	caseVariable := "lower_case_table_names"
	identifierCaseSensitive, err := driver.getServerVariable(ctx, caseVariable)
	if err != nil {
		return err
	}

	identifierCaseSensitiveValue, err := strconv.Atoi(identifierCaseSensitive)
	if err != nil {
		return err
	}

	var originalDBName string
	switch identifierCaseSensitiveValue {
	case LetterCaseOnDiskLetterCaseCmp:
		originalDBName = originalDatabase
	case LowerCaseOnDiskLowerCaseCmp:
		originalDBName = strings.ToLower(originalDatabase)
	case LetterCaseOnDiskLowerCaseCmp:
		originalDBName = strings.ToLower(originalDatabase)
	default:
		return errors.Errorf("expecting value of %s in range [%d, %d, %d], but get %s", caseVariable, 0, 1, 2, identifierCaseSensitive)
	}

	// Extract the SQL statements from the binlog and replay them to the pitrDatabase via the mysql client by pipe.
	mysqlbinlogArgs := []string{
		// Verify checksum binlog events.
		"--verify-binlog-checksum",
		// Disable binary logging.
		"--disable-log-bin",
		// Create rewrite rules for databases when playing back from logs written in row-based format, so that we can apply the binlog to PITR database instead of the original database.
		"--rewrite-db", fmt.Sprintf("%s->%s", originalDBName, targetDatabase),
		// List entries for just this database. It's applied after the --rewrite-db option, so we should provide the rewritten database, i.e., pitrDatabase.
		"--database", targetDatabase,
		// Start decoding the binary log at the log position, this option applies to the first log file named on the command line.
		"--start-position", fmt.Sprintf("%d", startBinlogInfo.Position),
		// Stop reading the binary log at the first event having a timestamp equal to or later than the datetime argument.
		"--stop-datetime", formatDateTime(targetTs),
	}

	mysqlbinlogArgs = append(mysqlbinlogArgs, replayBinlogPaths...)

	mysqlArgs := []string{
		"--host", driver.connCfg.Host,
		"--user", driver.connCfg.Username,
	}
	if driver.connCfg.Port != "" {
		mysqlArgs = append(mysqlArgs, "--port", driver.connCfg.Port)
	}
	if driver.connCfg.Password != "" {
		// The --password parameter of mysql/mysqlbinlog does not support the "--password PASSWORD" format (split by space).
		// If provided like that, the program will hang.
		mysqlArgs = append(mysqlArgs, fmt.Sprintf("--password=%s", driver.connCfg.Password))
	}

	mysqlbinlogCmd := exec.CommandContext(ctx, mysqlutil.GetPath(mysqlutil.MySQLBinlog, driver.dbBinDir), mysqlbinlogArgs...)
	mysqlCmd := exec.CommandContext(ctx, mysqlutil.GetPath(mysqlutil.MySQL, driver.dbBinDir), mysqlArgs...)
	slog.Debug("Start replay binlog commands.",
		slog.String("mysqlbinlog", mysqlbinlogCmd.String()),
		slog.String("mysql", mysqlCmd.String()))

	mysqlRead, err := mysqlbinlogCmd.StdoutPipe()
	if err != nil {
		return errors.Wrap(err, "cannot get mysqlbinlog stdout pipe")
	}
	defer mysqlRead.Close()

	mysqlbinlogCmd.Stderr = os.Stderr

	countingReader := common.NewCountingReader(mysqlRead)
	mysqlCmd.Stderr = os.Stderr
	mysqlCmd.Stdout = os.Stderr
	mysqlCmd.Stdin = countingReader
	driver.replayedBinlogBytes = countingReader

	if err := mysqlbinlogCmd.Start(); err != nil {
		return errors.Wrap(err, "cannot start mysqlbinlog command")
	}
	if err := mysqlCmd.Run(); err != nil {
		return errors.Wrap(err, "mysql command fails")
	}
	if err := mysqlbinlogCmd.Wait(); err != nil {
		return errors.Wrap(err, "error occurred while waiting for mysqlbinlog to exit")
	}

	slog.Debug("Replayed binlog successfully.")
	return nil
}

// GetRestoredBackupBytes gets the restored backup bytes.
func (driver *Driver) GetRestoredBackupBytes() int64 {
	if driver.restoredBackupBytes == nil {
		return 0
	}
	return driver.restoredBackupBytes.Count()
}

// GetReplayedBinlogBytes gets the replayed binlog bytes.
func (driver *Driver) GetReplayedBinlogBytes() int64 {
	if driver.replayedBinlogBytes == nil {
		return 0
	}
	return driver.replayedBinlogBytes.Count()
}

// ReplayBinlogToDatabase replays the binlog of originDatabaseName to the targetDatabaseName.
func (driver *Driver) ReplayBinlogToDatabase(ctx context.Context, originDatabaseName, targetDatabaseName string, startBinlogInfo, targetBinlogInfo api.BinlogInfo, targetTs int64, binlogDir string) error {
	return driver.replayBinlogFromDir(ctx, originDatabaseName, targetDatabaseName, startBinlogInfo, targetBinlogInfo, targetTs, binlogDir)
}

// ReplayBinlogToPITRDatabase replays binlog to the PITR database.
// It's the second step of the PITR process.
func (driver *Driver) ReplayBinlogToPITRDatabase(ctx context.Context, databaseName string, startBinlogInfo, targetBinlogInfo api.BinlogInfo, suffixTs, targetTs int64) error {
	pitrDatabaseName := util.GetPITRDatabaseName(databaseName, suffixTs)
	return driver.replayBinlogFromDir(ctx, databaseName, pitrDatabaseName, startBinlogInfo, targetBinlogInfo, targetTs, driver.binlogDir)
}

// RestoreBackupToDatabase create the database named `databaseName` and restores a full backup to the given database.
func (driver *Driver) RestoreBackupToDatabase(ctx context.Context, backup io.Reader, databaseName string) error {
	if err := driver.restoreImpl(ctx, backup, databaseName); err != nil {
		return errors.Wrapf(err, "failed to restore backup to the database %s", databaseName)
	}
	return nil
}

// RestoreBackupToPITRDatabase restores a full backup to the PITR database.
// It's the first step of the PITR process.
func (driver *Driver) RestoreBackupToPITRDatabase(ctx context.Context, backup io.Reader, databaseName string, suffixTs int64) error {
	pitrDatabaseName := util.GetPITRDatabaseName(databaseName, suffixTs)
	// If there's already a PITR database, it means there's a failed trial before this task execution.
	// We need to clean up the dirty state and start clean for idempotent task execution.
	stmt := fmt.Sprintf("DROP DATABASE IF EXISTS `%s`; CREATE DATABASE `%s`;", pitrDatabaseName, pitrDatabaseName)
	db := driver.GetDB()
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return errors.Wrapf(err, "failed to create the PITR database %s", pitrDatabaseName)
	}
	if err := driver.restoreImpl(ctx, backup, pitrDatabaseName); err != nil {
		return errors.Wrapf(err, "failed to restore backup to the PITR database %s", pitrDatabaseName)
	}
	return nil
}

// GetBinlogReplayList returns the path list of the binlog that need be replayed.
func GetBinlogReplayList(startBinlogInfo, targetBinlogInfo api.BinlogInfo, binlogDir string) ([]string, error) {
	_, startBinlogSeq, err := ParseBinlogName(startBinlogInfo.FileName)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot parse the start binlog file name %q", startBinlogInfo.FileName)
	}
	_, targetBinlogSeq, err := ParseBinlogName(targetBinlogInfo.FileName)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot parse the target binlog file name %q", targetBinlogInfo.FileName)
	}

	metaList, err := getSortedLocalBinlogFilesMeta(binlogDir)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to read local binlog metadata files from directory %s", binlogDir)
	}

	metaToReplay, err := getMetaReplayList(metaList, startBinlogSeq, targetBinlogSeq)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get slice of binlog metadata file between seq %d and %d in directory %s", startBinlogSeq, targetBinlogSeq, binlogDir)
	}

	if !binlogMetaAreContinuous(metaToReplay) {
		return nil, errors.Errorf("discontinuous binlog file extensions detected between seq %d and %d in directory %s", startBinlogSeq, targetBinlogSeq, binlogDir)
	}

	var binlogReplayList []string
	for _, meta := range metaToReplay {
		binlogReplayList = append(binlogReplayList, filepath.Join(binlogDir, meta.binlogName))
	}

	return binlogReplayList, nil
}

func getMetaReplayList(metaList []binlogFileMeta, startSeq, targetSeq int64) ([]binlogFileMeta, error) {
	startIndex, err := findBinlogSeqIndex(metaList, startSeq)
	if err != nil {
		return nil, errors.Errorf("failed to find the starting local binlog metadata file with seq %d", startSeq)
	}
	targetIndex, err := findBinlogSeqIndex(metaList, targetSeq)
	if err != nil {
		return nil, errors.Errorf("failed to find the target local binlog metadata file with seq %d", targetSeq)
	}
	if startIndex > targetIndex {
		return nil, errors.Errorf("start index %d must be less than target index %d", startIndex, targetIndex)
	}
	return metaList[startIndex : targetIndex+1], nil
}

func findBinlogSeqIndex(metaList []binlogFileMeta, seq int64) (int, error) {
	for i, meta := range metaList {
		if meta.seq == seq {
			return i, nil
		}
	}
	return 0, errors.Errorf("failed to find index with seq %d in binlog metadata list", seq)
}

// sortBinlogFiles will sort binlog files in ascending order by their numeric extension.
// For mysql binlog, after the serial number reaches 999999, the next serial number will not return to 000000, but 1000000,
// so we cannot directly use string to compare lexicographical order.
func sortBinlogFiles(binlogFiles []BinlogFile) []BinlogFile {
	var sorted []BinlogFile
	sorted = append(sorted, binlogFiles...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Seq < sorted[j].Seq
	})
	return sorted
}

// SwapPITRDatabase renames the pitr database to the target, and the original to the old database
// It returns the pitr and old database names after swap.
// It performs the step 2 of the restore process.
func SwapPITRDatabase(ctx context.Context, conn *sql.Conn, database string, suffixTs int64) (string, string, error) {
	pitrDatabaseName := util.GetPITRDatabaseName(database, suffixTs)
	pitrOldDatabase := util.GetPITROldDatabaseName(database, suffixTs)

	// Handle the case that the original database does not exist, because user could drop a database and want to restore it.
	slog.Debug("Checking database exists.", slog.String("database", database))
	dbExists, err := databaseExists(ctx, conn, database)
	if err != nil {
		return pitrDatabaseName, pitrOldDatabase, errors.Wrapf(err, "failed to check whether database %q exists", database)
	}

	slog.Debug("Turning binlog OFF.")
	// Set OFF the session variable sql_log_bin so that the writes in the following SQL statements will not be recorded in the binlog.
	if _, err := conn.ExecContext(ctx, "SET sql_log_bin=OFF"); err != nil {
		return pitrDatabaseName, pitrOldDatabase, err
	}

	if !dbExists {
		slog.Debug("Database does not exist, creating...", slog.String("database", database))
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE `%s`", database)); err != nil {
			return pitrDatabaseName, pitrOldDatabase, errors.Wrapf(err, "failed to create non-exist database %q", database)
		}
	}

	slog.Debug("Getting tables in the original and PITR databases.")
	tables, err := getTables(ctx, conn, database)
	if err != nil {
		return pitrDatabaseName, pitrOldDatabase, errors.Wrapf(err, "failed to get tables of database %q", database)
	}
	tablesPITR, err := getTables(ctx, conn, pitrDatabaseName)
	if err != nil {
		return pitrDatabaseName, pitrOldDatabase, errors.Wrapf(err, "failed to get tables of database %q", pitrDatabaseName)
	}

	if len(tables) == 0 && len(tablesPITR) == 0 {
		slog.Warn("Both databases are empty, skip renaming tables",
			slog.String("originalDatabase", database),
			slog.String("pitrDatabase", pitrDatabaseName))
		return pitrDatabaseName, pitrOldDatabase, nil
	}

	if _, err := conn.ExecContext(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS `%s`; CREATE DATABASE `%s`;", pitrOldDatabase, pitrOldDatabase)); err != nil {
		return pitrDatabaseName, pitrOldDatabase, err
	}

	var tableRenames []string
	for _, table := range tables {
		tableRenames = append(tableRenames, fmt.Sprintf("`%s`.`%s` TO `%s`.`%s`", database, table.Name, pitrOldDatabase, table.Name))
	}
	for _, table := range tablesPITR {
		tableRenames = append(tableRenames, fmt.Sprintf("`%s`.`%s` TO `%s`.`%s`", pitrDatabaseName, table.Name, database, table.Name))
	}
	renameStmt := fmt.Sprintf("RENAME TABLE %s;", strings.Join(tableRenames, ", "))
	slog.Debug("generated RENAME TABLE statement", slog.String("stmt", renameStmt))

	if _, err := conn.ExecContext(ctx, renameStmt); err != nil {
		return pitrDatabaseName, pitrOldDatabase, err
	}

	if _, err := conn.ExecContext(ctx, "SET sql_log_bin=ON"); err != nil {
		return pitrDatabaseName, pitrOldDatabase, err
	}

	return pitrDatabaseName, pitrOldDatabase, nil
}

// getTables gets all tables of a database.
func getTables(ctx context.Context, conn *sql.Conn, dbName string) ([]*TableSchema, error) {
	txn, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	return getTablesTx(txn, storepb.Engine_MYSQL, dbName)
}

func databaseExists(ctx context.Context, conn *sql.Conn, database string) (bool, error) {
	query := fmt.Sprintf("SELECT 1 FROM INFORMATION_SCHEMA.SCHEMATA WHERE SCHEMA_NAME='%s'", database)
	var unused string
	if err := conn.QueryRowContext(ctx, query).Scan(&unused); err != nil {
		if err == sql.ErrNoRows {
			// The query returns empty row, which means there's no such database.
			return false, nil
		}
		return false, util.FormatErrorWithQuery(err, query)
	}
	return true, nil
}

func getSortedLocalBinlogFilesMeta(binlogDir string) ([]binlogFileMeta, error) {
	metaFileInfoListLocal, err := os.ReadDir(binlogDir)
	if err != nil {
		return nil, err
	}

	var metaList []binlogFileMeta
	for _, fileInfo := range metaFileInfoListLocal {
		if !strings.HasSuffix(fileInfo.Name(), binlogMetaSuffix) {
			continue
		}
		meta, err := readBinlogMetaFile(binlogDir, fileInfo.Name())
		if err != nil {
			return nil, err
		}
		metaList = append(metaList, meta)
	}

	return sortBinlogFilesMeta(metaList), nil
}

func sortBinlogFilesMeta(binlogFilesMeta []binlogFileMeta) []binlogFileMeta {
	var sorted []binlogFileMeta
	sorted = append(sorted, binlogFilesMeta...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].seq < sorted[j].seq
	})
	return sorted
}

// GetSortedLocalBinlogFiles returns a sorted BinlogFile list in the given binlog dir.
func (driver *Driver) GetSortedLocalBinlogFiles() ([]BinlogFile, error) {
	binlogFilesInfoLocal, err := os.ReadDir(driver.binlogDir)
	if err != nil {
		return nil, err
	}
	var binlogFilesLocal []BinlogFile
	// TODO(dragonly): Get binlog files according to the metadata files.
	for _, fileInfo := range binlogFilesInfoLocal {
		if strings.HasSuffix(fileInfo.Name(), binlogMetaSuffix) {
			continue
		}
		fi, err := fileInfo.Info()
		if err != nil {
			return nil, errors.Wrapf(err, "cannot get file info %s", fileInfo.Name())
		}
		binlogFile, err := newBinlogFile(fileInfo.Name(), fi.Size())
		if err != nil {
			return nil, err
		}
		binlogFilesLocal = append(binlogFilesLocal, binlogFile)
	}
	return sortBinlogFiles(binlogFilesLocal), nil
}

func binlogMetaAreContinuous(files []binlogFileMeta) bool {
	for i := 0; i < len(files)-1; i++ {
		if files[i].seq+1 != files[i+1].seq {
			return false
		}
	}
	return true
}

// Download binlog files on server.
func (driver *Driver) downloadBinlogFilesOnServer(ctx context.Context, metaList []binlogFileMeta, binlogFilesOnServerSorted []BinlogFile, downloadLatestBinlogFile bool, uploader *bbs3.Client) error {
	if len(binlogFilesOnServerSorted) == 0 {
		slog.Debug("No binlog file found on server to download")
		return nil
	}
	latestBinlogFileOnServer := binlogFilesOnServerSorted[len(binlogFilesOnServerSorted)-1]
	metaMap := make(map[int64]bool)
	for _, meta := range metaList {
		metaMap[meta.seq] = true
	}
	for _, fileOnServer := range binlogFilesOnServerSorted {
		isLatest := fileOnServer.Name == latestBinlogFileOnServer.Name
		if isLatest && !downloadLatestBinlogFile {
			continue
		}
		_, exist := metaMap[fileOnServer.Seq]
		if !exist || isLatest {
			binlogFilePath := filepath.Join(driver.binlogDir, fileOnServer.Name)
			slog.Debug("Downloading binlog file from MySQL server.", slog.String("path", binlogFilePath), slog.Bool("isLatest", isLatest))
			if err := driver.downloadBinlogFile(ctx, fileOnServer, isLatest); err != nil {
				slog.Error("Failed to download binlog file", slog.String("path", binlogFilePath), log.BBError(err))
				return errors.Wrapf(err, "failed to download binlog file %q", binlogFilePath)
			}
			if err := driver.writeBinlogMetadataFile(ctx, fileOnServer.Name); err != nil {
				return errors.Wrapf(err, "failed to write binlog metadata file for binlog file %q", binlogFilePath)
			}
			if uploader != nil {
				if err := driver.uploadBinlogFileToCloud(ctx, uploader, fileOnServer.Name); err != nil {
					return errors.Wrapf(err, "failed to upload binlog file %q to cloud storage", binlogFilePath)
				}
			}
		}
	}
	return nil
}

// GetBinlogDir gets the binlogDir.
func (driver *Driver) GetBinlogDir() string {
	return driver.binlogDir
}

// FetchAllBinlogFiles downloads all binlog files on server to `binlogDir`.
func (driver *Driver) FetchAllBinlogFiles(ctx context.Context, downloadLatestBinlogFile bool, client *bbs3.Client) error {
	if err := os.MkdirAll(driver.binlogDir, os.ModePerm); err != nil {
		return errors.Wrapf(err, "failed to create binlog directory %q", driver.binlogDir)
	}
	// Read binlog files list on server.
	binlogFilesOnServerSorted, err := driver.GetSortedBinlogFilesOnServer(ctx)
	if err != nil {
		return err
	}
	if len(binlogFilesOnServerSorted) == 0 {
		slog.Debug("No binlog file found on server to download")
		return nil
	}

	if client != nil {
		if err := driver.syncBinlogMetaFileFromCloud(ctx, client); err != nil {
			return errors.Wrap(err, "failed to sync binlog metadata files from the cloud")
		}
	}

	metaList, err := getSortedLocalBinlogFilesMeta(driver.binlogDir)
	if err != nil {
		return errors.Wrap(err, "failed to read local binlog metadata files")
	}

	if err := driver.downloadBinlogFilesOnServer(ctx, metaList, binlogFilesOnServerSorted, downloadLatestBinlogFile, client); err != nil {
		return errors.Wrap(err, "failed to download binlog files from the MySQL server")
	}

	return nil
}

func (driver *Driver) syncBinlogMetaFileFromCloud(ctx context.Context, client *bbs3.Client) error {
	metaListToDownload, err := driver.getBinlogMetaFileListToDownload(ctx, client)
	if err != nil {
		return errors.Wrapf(err, "failed to get binlog metadata file list on cloud in directory %q", driver.binlogDir)
	}
	if len(metaListToDownload) == 0 {
		return nil
	}
	slog.Debug(fmt.Sprintf("Downloading %d binlog metadata file from cloud storage", len(metaListToDownload)))

	for _, metaFileName := range metaListToDownload {
		// Use filepath.Join to compose an OS-specific local file system path.
		filePathLocal := filepath.Join(driver.binlogDir, metaFileName)
		// Use path.Join to compose a path on cloud which always uses / as the separator.
		filePathOnCloud := path.Join(common.GetBinlogRelativeDir(driver.binlogDir), metaFileName)
		if err := client.DownloadFileFromCloud(ctx, filePathLocal, filePathOnCloud); err != nil {
			return errors.Wrapf(err, "failed to download binlog metadata file %s from the cloud storage", metaFileName)
		}
	}

	return nil
}

func (driver *Driver) getBinlogMetaFileListToDownload(ctx context.Context, client *bbs3.Client) ([]string, error) {
	listOutput, err := client.ListObjects(ctx, driver.binlogDir)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to list binlog dir %q in the cloud storage", driver.binlogDir)
	}
	var downloadList []string
	for _, item := range listOutput {
		binlogPathOnCloud := *item.Key
		if !strings.HasSuffix(binlogPathOnCloud, binlogMetaSuffix) {
			continue
		}
		binlogName := filepath.Base(binlogPathOnCloud)
		binlogPathLocal := filepath.Join(driver.binlogDir, binlogName)
		if _, err := os.Stat(binlogPathLocal); err != nil {
			if os.IsNotExist(err) {
				downloadList = append(downloadList, binlogName)
			} else {
				slog.Error("Failed to get stat of local binlog file", slog.String("path", binlogPathLocal))
			}
		}
	}
	return downloadList, nil
}

// Syncs the binlog specified by `meta` between the instance and local.
// If isLast is true, it means that this is the last binlog file containing the targetTs event.
// It may keep growing as there are ongoing writes to the database. So we just need to check that
// the file size is larger or equal to the binlog file size we queried from the MySQL server earlier.
func (driver *Driver) downloadBinlogFile(ctx context.Context, binlogFileToDownload BinlogFile, isLast bool) error {
	tempBinlogPrefix := filepath.Join(driver.binlogDir, "tmp-")
	// TODO(zp): support ssl?
	args := []string{
		binlogFileToDownload.Name,
		"--read-from-remote-server",
		// Verify checksum binlog events.
		"--verify-binlog-checksum",
		"--host", driver.connCfg.Host,
		"--user", driver.connCfg.Username,
		"--raw",
		// With --raw this is a prefix for the file names.
		"--result-file", tempBinlogPrefix,
	}
	if driver.connCfg.Port != "" {
		args = append(args, "--port", driver.connCfg.Port)
	}

	cmd := exec.CommandContext(ctx, mysqlutil.GetPath(mysqlutil.MySQLBinlog, driver.dbBinDir), args...)
	// We cannot set password as a flag. Otherwise, there is warning message
	// "mysqlbinlog: [Warning] Using a password on the command line interface can be insecure."
	if driver.connCfg.Password != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("MYSQL_PWD=%s", driver.connCfg.Password))
	}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	slog.Debug("Downloading binlog files using mysqlbinlog", slog.String("cmd", cmd.String()))
	binlogFilePathTemp := tempBinlogPrefix + binlogFileToDownload.Name
	defer os.Remove(binlogFilePathTemp)
	if err := cmd.Run(); err != nil {
		slog.Error("Failed to execute mysqlbinlog binary", log.BBError(err))
		return errors.Wrap(err, "failed to execute mysqlbinlog binary")
	}

	slog.Debug("Checking downloaded binlog file stat", slog.String("path", binlogFilePathTemp))
	binlogFileTempInfo, err := os.Stat(binlogFilePathTemp)
	if err != nil {
		slog.Error("Failed to get stat of the binlog file.", slog.String("path", binlogFilePathTemp), log.BBError(err))
		return errors.Wrapf(err, "failed to get stat of the binlog file %q", binlogFilePathTemp)
	}
	if !isLast && binlogFileTempInfo.Size() != binlogFileToDownload.Size {
		slog.Error("Downloaded archived binlog file size is not equal to size queried on the MySQL server earlier.",
			slog.String("binlog", binlogFileToDownload.Name),
			slog.Int64("sizeInfo", binlogFileToDownload.Size),
			slog.Int64("downloadedSize", binlogFileTempInfo.Size()),
		)
		return errors.Errorf("downloaded archived binlog file %q size %d is not equal to size %d queried on MySQL server earlier", binlogFilePathTemp, binlogFileTempInfo.Size(), binlogFileToDownload.Size)
	}

	binlogFilePath := filepath.Join(driver.binlogDir, binlogFileToDownload.Name)
	if err := os.Rename(binlogFilePathTemp, binlogFilePath); err != nil {
		return errors.Wrapf(err, "failed to rename %q to %q", binlogFilePathTemp, binlogFilePath)
	}

	if err := driver.writeBinlogMetadataFile(ctx, binlogFileToDownload.Name); err != nil {
		return errors.Wrapf(err, "failed to write binlog metadata file for binlog file %q", binlogFilePathTemp)
	}
	return nil
}

func (driver *Driver) uploadBinlogFileToCloud(ctx context.Context, uploader *bbs3.Client, binlogFileName string) error {
	binlogFilePath := filepath.Join(driver.binlogDir, binlogFileName)
	metaFileName := binlogFileName + binlogMetaSuffix
	metaFilePath := filepath.Join(driver.binlogDir, metaFileName)
	binlogFile, err := os.Open(binlogFilePath)
	if err != nil {
		return errors.Wrapf(err, "failed to open local binlog file %q for uploading", binlogFilePath)
	}
	defer binlogFile.Close()
	defer os.Remove(binlogFilePath)
	relativeDir := common.GetBinlogRelativeDir(driver.binlogDir)
	if _, err := uploader.UploadObject(ctx, path.Join(relativeDir, binlogFileName), binlogFile); err != nil {
		// Remove the local metadata file so that it can be re-uploaded later.
		if err := os.Remove(metaFilePath); err != nil {
			slog.Warn("Failed to remove binlog metadata file %q when error occurs in uploading binlog file", slog.String("binlogFile", binlogFilePath), log.BBError(err))
		}
		return errors.Wrapf(err, "failed to upload binlog file %q to cloud storage", binlogFileName)
	}

	metaFile, err := os.Open(metaFilePath)
	if err != nil {
		return errors.Wrapf(err, "failed to open local binlog metadata file %q for uploading", metaFilePath)
	}
	defer metaFile.Close()
	// We leave the local metadata file to indicate that the binlog file has been uploaded successfully.
	if _, err := uploader.UploadObject(ctx, path.Join(relativeDir, metaFileName), metaFile); err != nil {
		return errors.Wrapf(err, "failed to upload binlog metadata file %q to cloud storage", metaFileName)
	}
	slog.Debug("Successfully uploaded binlog file to cloud storage", slog.String("path", binlogFilePath))

	return nil
}

func (driver *Driver) writeBinlogMetadataFile(ctx context.Context, binlogFileName string) error {
	eventTs, err := driver.parseLocalBinlogFirstEventTs(ctx, binlogFileName)
	if err != nil {
		return errors.Wrapf(err, "failed to parse the local binlog file %q's first binlog event ts", binlogFileName)
	}
	metadataFilePath := filepath.Join(driver.binlogDir, binlogFileName+".meta")
	metadataFile, err := os.Create(metadataFilePath)
	if err != nil {
		return errors.Wrapf(err, "failed to create binlog metadata file %q", metadataFilePath)
	}
	metadata := binlogFileMeta{
		FirstEventTs: eventTs,
	}
	metadataBytes, err := json.Marshal(metadata)
	if err != nil {
		return errors.Wrapf(err, "failed to marshal binlog metadata %+v", metadata)
	}
	if _, err := metadataFile.Write(metadataBytes); err != nil {
		return errors.Wrapf(err, "failed to write binlog metadata file %q", metadataFilePath)
	}
	return nil
}

// GetSortedBinlogFilesOnServer returns the information of binlog files in ascending order by their numeric extension.
func (driver *Driver) GetSortedBinlogFilesOnServer(ctx context.Context) ([]BinlogFile, error) {
	db := driver.GetDB()
	query := "SHOW BINARY LOGS"
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, errors.Wrapf(err, "cannot get columns from %q query", query)
	}
	findFileName := false
	findFileSize := false
	for _, columnName := range columns {
		switch columnName {
		case "Log_name":
			findFileName = true
		case "File_size":
			findFileSize = true
		}
	}
	if !findFileName || !findFileSize {
		return nil, errors.Errorf("cannot find file name or size columns from %q query", query)
	}

	var binlogFiles []BinlogFile
	var unused any
	for rows.Next() {
		var name string
		var size int64
		cols := make([]any, len(columns))
		// The query SHOW BINARY LOGS returns uncertain number of columns because MySQL 5.7 and 8.0 produce different results.
		// So we have to dynamically scan the columns, and return the error if we cannot find the File and Position columns.
		for i := 0; i < len(columns); i++ {
			switch columns[i] {
			case "Log_name":
				cols[i] = &name
			case "File_size":
				cols[i] = &size
			default:
				cols[i] = &unused
			}
		}
		if err := rows.Scan(cols...); err != nil {
			return nil, errors.Wrapf(err, "cannot scan row from %q query", query)
		}

		binlogFile, err := newBinlogFile(name, size)
		if err != nil {
			return nil, err
		}
		binlogFiles = append(binlogFiles, binlogFile)
	}
	if err := rows.Err(); err != nil {
		return nil, util.FormatErrorWithQuery(err, query)
	}

	return sortBinlogFiles(binlogFiles), nil
}

func parseBinlogEventTsInLine(line string) (eventTs int64, found bool, err error) {
	// The target line starts with string like "#220421 14:49:26 server id 1"
	if !strings.Contains(line, "server id") {
		return 0, false, nil
	}
	if strings.Contains(line, "end_log_pos 0") {
		// https://github.com/mysql/mysql-server/blob/8.0/client/mysqlbinlog.cc#L1209-L1212
		// Fake events with end_log_pos=0 could be generated and we need to ignore them.
		return 0, false, nil
	}
	fields := strings.Fields(line)
	// fields should starts with ["#220421", "14:49:26", "server", "id", "1", "end_log_pos", "34794"]
	if len(fields) < 7 ||
		(len(fields[0]) != 7 || fields[2] != "server" || fields[3] != "id" || fields[5] != "end_log_pos") {
		return 0, false, errors.Errorf("found unexpected mysqlbinlog output line %q when parsing binlog event timestamp", line)
	}
	datetime, err := time.ParseInLocation("060102 15:04:05", fmt.Sprintf("%s %s", fields[0][1:], fields[1]), time.Local)
	if err != nil {
		return 0, false, err
	}
	return datetime.Unix(), true, nil
}

func parseBinlogEventPosInLine(line string) (pos int64, found bool, err error) {
	// The mysqlbinlog output will contains a line starting with "# at 35065", which is the binlog event's start position.
	if !strings.HasPrefix(line, "# at ") {
		return 0, false, nil
	}
	// This is the line containing the start position of the binlog event.
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return 0, false, errors.Errorf("unexpected mysqlbinlog output line %q when parsing binlog event start position", line)
	}
	pos, err = strconv.ParseInt(fields[2], 10, 0)
	if err != nil {
		return 0, false, err
	}
	return pos, true, nil
}

// Parse the first binlog eventTs of a local binlog file.
func (driver *Driver) parseLocalBinlogFirstEventTs(ctx context.Context, fileName string) (int64, error) {
	args := []string{
		// Local binlog file path.
		path.Join(driver.binlogDir, fileName),
		// Verify checksum binlog events.
		"--verify-binlog-checksum",
		// Tell mysqlbinlog to suppress the BINLOG statements for row events, which reduces the unneeded output.
		"--base64-output=DECODE-ROWS",
	}
	cmd := exec.CommandContext(ctx, mysqlutil.GetPath(mysqlutil.MySQLBinlog, driver.dbBinDir), args...)
	cmd.Stderr = os.Stderr
	pr, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	s := bufio.NewScanner(pr)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	defer func() {
		_ = pr.Close()
		_ = cmd.Process.Kill()
	}()

	var eventTs int64
	for s.Scan() {
		line := s.Text()
		eventTsParsed, found, err := parseBinlogEventTsInLine(line)
		if err != nil {
			return 0, errors.Wrap(err, "failed to parse binlog eventTs from mysqlbinlog output")
		}
		if !found {
			continue
		}
		eventTs = eventTsParsed
		break
	}

	return eventTs, nil
}

// ParseBinlogName parses the numeric extension and the binary log base name by using split the dot.
// Examples:
//   - ("binlog.000001") => ("binlog", 1)
//   - ("binlog000001") => ("", err)
func ParseBinlogName(name string) (string, int64, error) {
	s := strings.Split(name, ".")
	if len(s) != 2 {
		return "", 0, ErrParseBinlogName{err: errors.Errorf("failed to parse binlog extension, expecting two parts in the binlog file name %q but got %d", name, len(s))}
	}
	seq, err := strconv.ParseInt(s[1], 10, 0)
	if err != nil {
		return "", 0, ErrParseBinlogName{err: errors.Wrapf(err, "failed to parse the sequence number %s", s[1])}
	}
	return s[0], seq, nil
}

// GenBinlogFileNames generates the binlog file names between the start end end sequence numbers.
// The generation algorithm refers to the implementation of mysql-server: https://sourcegraph.com/github.com/mysql/mysql-server@a246bad76b9271cb4333634e954040a970222e0a/-/blob/sql/binlog.cc?L3693.
func GenBinlogFileNames(base string, seqStart, seqEnd int64) []string {
	var ret []string
	for i := seqStart; i <= seqEnd; i++ {
		ret = append(ret, fmt.Sprintf("%s.%06d", base, i))
	}
	return ret
}

func (driver *Driver) getServerVariable(ctx context.Context, varName string) (string, error) {
	db := driver.GetDB()
	query := fmt.Sprintf("SHOW VARIABLES LIKE '%s'", varName)
	var varNameFound, value string
	if err := db.QueryRowContext(ctx, query).Scan(&varNameFound, &value); err != nil {
		if err == sql.ErrNoRows {
			return "", common.FormatDBErrorEmptyRowWithQuery(query)
		}
		return "", util.FormatErrorWithQuery(err, query)
	}
	if varName != varNameFound {
		return "", errors.Errorf("expecting variable %s, but got %s", varName, varNameFound)
	}
	return value, nil
}

// CheckBinlogEnabled checks whether binlog is enabled for the current instance.
func (driver *Driver) CheckBinlogEnabled(ctx context.Context) error {
	value, err := driver.getServerVariable(ctx, "log_bin")
	if err != nil {
		return err
	}
	if strings.ToUpper(value) != "ON" {
		return errors.Errorf("binlog is not enabled")
	}
	return nil
}

// CheckBinlogRowFormat checks whether the binlog format is ROW.
func (driver *Driver) CheckBinlogRowFormat(ctx context.Context) error {
	value, err := driver.getServerVariable(ctx, "binlog_format")
	if err != nil {
		return err
	}
	if strings.ToUpper(value) != "ROW" {
		return errors.Errorf("binlog format is not ROW but %s", value)
	}
	return nil
}

// formatDateTime formats the timestamp to the local time string.
func formatDateTime(ts int64) string {
	t := time.Unix(ts, 0)
	return fmt.Sprintf("%d-%d-%d %d:%d:%d", t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second())
}
