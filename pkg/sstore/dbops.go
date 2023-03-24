package sstore

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/sawka/txwrap"
	"github.com/scripthaus-dev/mshell/pkg/base"
	"github.com/scripthaus-dev/mshell/pkg/packet"
	"github.com/scripthaus-dev/mshell/pkg/shexec"
	"github.com/scripthaus-dev/sh2-server/pkg/scbase"
)

const HistoryCols = "h.historyid, h.ts, h.userid, h.sessionid, h.screenid, h.lineid, h.cmdid, h.haderror, h.cmdstr, h.remoteownerid, h.remoteid, h.remotename, h.ismetacmd, h.incognito, h.linenum"
const DefaultMaxHistoryItems = 1000

type SingleConnDBGetter struct {
	SingleConnLock *sync.Mutex
}

type TxWrap = txwrap.TxWrap

var dbWrap *SingleConnDBGetter

func init() {
	dbWrap = &SingleConnDBGetter{SingleConnLock: &sync.Mutex{}}
}

func (dbg *SingleConnDBGetter) GetDB(ctx context.Context) (*sqlx.DB, error) {
	db, err := GetDB(ctx)
	if err != nil {
		return nil, err
	}
	dbg.SingleConnLock.Lock()
	return db, nil
}

func (dbg *SingleConnDBGetter) ReleaseDB(db *sqlx.DB) {
	dbg.SingleConnLock.Unlock()
}

func WithTx(ctx context.Context, fn func(tx *TxWrap) error) error {
	return txwrap.DBGWithTx(ctx, dbWrap, fn)
}

func NumSessions(ctx context.Context) (int, error) {
	var numSessions int
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := "SELECT count(*) FROM session"
		numSessions = tx.GetInt(query)
		return nil
	})
	return numSessions, txErr
}

func GetAllRemotes(ctx context.Context) ([]*RemoteType, error) {
	var rtn []*RemoteType
	err := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT * FROM remote ORDER BY remoteidx`
		marr := tx.SelectMaps(query)
		for _, m := range marr {
			rtn = append(rtn, FromMap[*RemoteType](m))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rtn, nil
}

func GetRemoteByAlias(ctx context.Context, alias string) (*RemoteType, error) {
	var remote *RemoteType
	err := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT * FROM remote WHERE remotealias = ?`
		m := tx.GetMap(query, alias)
		remote = FromMap[*RemoteType](m)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return remote, nil
}

func GetRemoteById(ctx context.Context, remoteId string) (*RemoteType, error) {
	var remote *RemoteType
	err := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT * FROM remote WHERE remoteid = ?`
		m := tx.GetMap(query, remoteId)
		remote = FromMap[*RemoteType](m)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return remote, nil
}

func GetLocalRemote(ctx context.Context) (*RemoteType, error) {
	var remote *RemoteType
	err := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT * FROM remote WHERE local`
		m := tx.GetMap(query)
		remote = FromMap[*RemoteType](m)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return remote, nil
}

func GetRemoteByCanonicalName(ctx context.Context, cname string) (*RemoteType, error) {
	var remote *RemoteType
	err := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT * FROM remote WHERE remotecanonicalname = ?`
		remote = GetMapGen[*RemoteType](tx, query, cname)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return remote, nil
}

func GetRemoteByPhysicalId(ctx context.Context, physicalId string) (*RemoteType, error) {
	var remote *RemoteType
	err := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT * FROM remote WHERE physicalid = ?`
		remote = GetMapGen[*RemoteType](tx, query, physicalId)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return remote, nil
}

func UpsertRemote(ctx context.Context, r *RemoteType) error {
	if r == nil {
		return fmt.Errorf("cannot insert nil remote")
	}
	if r.RemoteId == "" {
		return fmt.Errorf("cannot insert remote without id")
	}
	if r.RemoteCanonicalName == "" {
		return fmt.Errorf("cannot insert remote with canonicalname")
	}
	if r.RemoteType == "" {
		return fmt.Errorf("cannot insert remote without type")
	}
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT remoteid FROM remote WHERE remoteid = ?`
		if tx.Exists(query, r.RemoteId) {
			tx.Exec(`DELETE FROM remote WHERE remoteid = ?`, r.RemoteId)
		}
		query = `SELECT remoteid FROM remote WHERE remotecanonicalname = ?`
		if tx.Exists(query, r.RemoteCanonicalName) {
			return fmt.Errorf("remote has duplicate canonicalname '%s', cannot create", r.RemoteCanonicalName)
		}
		query = `SELECT remoteid FROM remote WHERE remotealias = ?`
		if r.RemoteAlias != "" && tx.Exists(query, r.RemoteAlias) {
			return fmt.Errorf("remote has duplicate alias '%s', cannot create", r.RemoteAlias)
		}
		query = `SELECT COALESCE(max(remoteidx), 0) FROM remote`
		maxRemoteIdx := tx.GetInt(query)
		r.RemoteIdx = int64(maxRemoteIdx + 1)
		query = `INSERT INTO remote
            ( remoteid, physicalid, remotetype, remotealias, remotecanonicalname, remotesudo, remoteuser, remotehost, connectmode, autoinstall, sshopts, remoteopts, lastconnectts, archived, remoteidx, local) VALUES
            (:remoteid,:physicalid,:remotetype,:remotealias,:remotecanonicalname,:remotesudo,:remoteuser,:remotehost,:connectmode,:autoinstall,:sshopts,:remoteopts,:lastconnectts,:archived,:remoteidx,:local)`
		tx.NamedExec(query, r.ToMap())
		return nil
	})
	return txErr
}

func InsertHistoryItem(ctx context.Context, hitem *HistoryItemType) error {
	if hitem == nil {
		return fmt.Errorf("cannot insert nil history item")
	}
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `INSERT INTO history 
                  ( historyid, ts, userid, sessionid, screenid, lineid, cmdid, haderror, cmdstr, remoteownerid, remoteid, remotename, ismetacmd, incognito, linenum) VALUES
                  (:historyid,:ts,:userid,:sessionid,:screenid,:lineid,:cmdid,:haderror,:cmdstr,:remoteownerid,:remoteid,:remotename,:ismetacmd,:incognito,:linenum)`
		tx.NamedExec(query, hitem.ToMap())
		return nil
	})
	return txErr
}

func IsIncognitoScreen(ctx context.Context, sessionId string, screenId string) (bool, error) {
	return false, nil
}

const HistoryQueryChunkSize = 1000

func _getNextHistoryItem(items []*HistoryItemType, index int, filterFn func(*HistoryItemType) bool) (*HistoryItemType, int) {
	for ; index < len(items); index++ {
		item := items[index]
		if filterFn(item) {
			return item, index
		}
	}
	return nil, index
}

// returns true if done, false if we still need to process more items
func (result *HistoryQueryResult) processItem(item *HistoryItemType, rawOffset int) bool {
	if result.prevItems < result.Offset {
		result.prevItems++
		return false
	}
	if len(result.Items) == result.MaxItems {
		result.HasMore = true
		result.NextRawOffset = rawOffset
		return true
	}
	if len(result.Items) == 0 {
		result.RawOffset = rawOffset
	}
	result.Items = append(result.Items, item)
	return false
}

func runHistoryQueryWithFilter(tx *TxWrap, opts HistoryQueryOpts) (*HistoryQueryResult, error) {
	if opts.MaxItems == 0 {
		return nil, fmt.Errorf("invalid query, maxitems is 0")
	}
	rtn := &HistoryQueryResult{Offset: opts.Offset, MaxItems: opts.MaxItems}
	var rawOffset int
	if opts.RawOffset >= opts.Offset {
		rtn.prevItems = opts.Offset
		rawOffset = opts.RawOffset
	} else {
		rawOffset = 0
	}
	for {
		resultItems, err := runHistoryQuery(tx, opts, rawOffset, HistoryQueryChunkSize)
		if err != nil {
			return nil, err
		}
		isDone := false
		for resultIdx := 0; resultIdx < len(resultItems); resultIdx++ {
			if opts.FilterFn != nil && !opts.FilterFn(resultItems[resultIdx]) {
				continue
			}
			isDone = rtn.processItem(resultItems[resultIdx], rawOffset+resultIdx)
			if isDone {
				break
			}
		}
		if isDone {
			break
		}
		if len(resultItems) < HistoryQueryChunkSize {
			break
		}
		rawOffset += HistoryQueryChunkSize
	}
	return rtn, nil
}

func runHistoryQuery(tx *TxWrap, opts HistoryQueryOpts, realOffset int, itemLimit int) ([]*HistoryItemType, error) {
	// check sessionid/screenid format because we are directly inserting them into the SQL
	if opts.SessionId != "" {
		_, err := uuid.Parse(opts.SessionId)
		if err != nil {
			return nil, fmt.Errorf("malformed sessionid")
		}
	}
	if opts.ScreenId != "" {
		_, err := uuid.Parse(opts.ScreenId)
		if err != nil {
			return nil, fmt.Errorf("malformed screenid")
		}
	}
	if opts.RemoteId != "" {
		_, err := uuid.Parse(opts.RemoteId)
		if err != nil {
			return nil, fmt.Errorf("malformed remoteid")
		}
	}
	whereClause := "WHERE 1"
	var queryArgs []interface{}
	hNumStr := ""
	if opts.SessionId != "" && opts.ScreenId != "" {
		whereClause += fmt.Sprintf(" AND h.sessionid = '%s' AND h.screenid = '%s'", opts.SessionId, opts.ScreenId)
		hNumStr = ""
	} else if opts.SessionId != "" {
		whereClause += fmt.Sprintf(" AND h.sessionid = '%s'", opts.SessionId)
		hNumStr = "s"
	} else {
		hNumStr = "g"
	}
	if opts.SearchText != "" {
		whereClause += " AND h.cmdstr LIKE ? ESCAPE '\\'"
		likeArg := opts.SearchText
		likeArg = strings.ReplaceAll(likeArg, "%", "\\%")
		likeArg = strings.ReplaceAll(likeArg, "_", "\\_")
		queryArgs = append(queryArgs, "%"+likeArg+"%")
	}
	if opts.FromTs > 0 {
		whereClause += fmt.Sprintf(" AND h.ts <= %d", opts.FromTs)
	}
	if opts.RemoteId != "" {
		whereClause += fmt.Sprintf(" AND h.remoteid = '%s'", opts.RemoteId)
	}
	if opts.NoMeta {
		whereClause += " AND NOT h.ismetacmd"
	}
	query := fmt.Sprintf("SELECT %s, ('%s' || CAST((row_number() OVER win) as text)) historynum FROM history h %s WINDOW win AS (ORDER BY h.ts, h.historyid) ORDER BY h.ts DESC, h.historyid DESC LIMIT %d OFFSET %d", HistoryCols, hNumStr, whereClause, itemLimit, realOffset)
	marr := tx.SelectMaps(query, queryArgs...)
	rtn := make([]*HistoryItemType, len(marr))
	for idx, m := range marr {
		hitem := FromMap[*HistoryItemType](m)
		rtn[idx] = hitem
	}
	return rtn, nil
}

func GetHistoryItems(ctx context.Context, opts HistoryQueryOpts) (*HistoryQueryResult, error) {
	var rtn *HistoryQueryResult
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		var err error
		rtn, err = runHistoryQueryWithFilter(tx, opts)
		if err != nil {
			return err
		}
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	return rtn, nil
}

func GetHistoryItemByLineNum(ctx context.Context, screenId string, lineNum int) (*HistoryItemType, error) {
	return WithTxRtn(ctx, func(tx *TxWrap) (*HistoryItemType, error) {
		query := `SELECT * FROM history WHERE screenid = ? AND linenum = ?`
		hitem := GetMapGen[*HistoryItemType](tx, query, screenId, lineNum)
		return hitem, nil
	})
}

func GetLastHistoryLineNum(ctx context.Context, screenId string) (int, error) {
	return WithTxRtn(ctx, func(tx *TxWrap) (int, error) {
		query := `SELECT COALESCE(max(linenum), 0) FROM history WHERE screenid = ?`
		maxLineNum := tx.GetInt(query, screenId)
		return maxLineNum, nil
	})
}

// includes archived sessions
func GetBareSessions(ctx context.Context) ([]*SessionType, error) {
	var rtn []*SessionType
	err := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT * FROM session ORDER BY archived, sessionidx, archivedts`
		tx.Select(&rtn, query)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rtn, nil
}

// does not include archived, finds lowest sessionidx (for resetting active session)
func GetFirstSessionId(ctx context.Context) (string, error) {
	var rtn []string
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT sessionid from session WHERE NOT archived ORDER by sessionidx`
		rtn = tx.SelectStrings(query)
		return nil
	})
	if txErr != nil {
		return "", txErr
	}
	if len(rtn) == 0 {
		return "", nil
	}
	return rtn[0], nil
}

func GetBareSessionById(ctx context.Context, sessionId string) (*SessionType, error) {
	var rtn SessionType
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT * FROM session WHERE sessionid = ?`
		tx.Get(&rtn, query, sessionId)
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	if rtn.SessionId == "" {
		return nil, nil
	}
	return &rtn, nil
}

func GetAllSessions(ctx context.Context) (*ModelUpdate, error) {
	return WithTxRtn(ctx, func(tx *TxWrap) (*ModelUpdate, error) {
		update := &ModelUpdate{}
		query := `SELECT * FROM session ORDER BY archived, sessionidx, archivedts`
		tx.Select(&update.Sessions, query)
		sessionMap := make(map[string]*SessionType)
		for _, session := range update.Sessions {
			sessionMap[session.SessionId] = session
			session.Full = true
		}
		query = `SELECT * FROM screen ORDER BY archived, screenidx, archivedts`
		update.Screens = SelectMapsGen[*ScreenType](tx, query)
		for _, screen := range update.Screens {
			screen.Full = true
		}
		query = `SELECT * FROM remote_instance`
		riArr := SelectMapsGen[*RemoteInstance](tx, query)
		for _, ri := range riArr {
			s := sessionMap[ri.SessionId]
			if s != nil {
				s.Remotes = append(s.Remotes, ri)
			}
		}
		query = `SELECT activesessionid FROM client`
		update.ActiveSessionId = tx.GetString(query)
		return update, nil
	})
}

func GetScreenLinesById(ctx context.Context, screenId string) (*ScreenLinesType, error) {
	return WithTxRtn(ctx, func(tx *TxWrap) (*ScreenLinesType, error) {
		query := `SELECT screenid FROM screen WHERE screenid = ?`
		screen := GetMappable[*ScreenLinesType](tx, query, screenId)
		if screen == nil {
			return nil, nil
		}
		query = `SELECT * FROM line WHERE screenid = ? ORDER BY linenum`
		tx.Select(&screen.Lines, query, screen.ScreenId)
		query = `SELECT * FROM cmd WHERE cmdid IN (SELECT cmdid FROM line WHERE screenid = ?)`
		screen.Cmds = SelectMapsGen[*CmdType](tx, query, screen.ScreenId)
		return screen, nil
	})
}

// includes archived screens
func GetSessionScreens(ctx context.Context, sessionId string) ([]*ScreenType, error) {
	return WithTxRtn(ctx, func(tx *TxWrap) ([]*ScreenType, error) {
		query := `SELECT * FROM screen WHERE sessionid = ? ORDER BY archived, screenidx, archivedts`
		rtn := SelectMapsGen[*ScreenType](tx, query, sessionId)
		for _, screen := range rtn {
			screen.Full = true
		}
		return rtn, nil
	})
}

func GetSessionById(ctx context.Context, id string) (*SessionType, error) {
	allSessionsUpdate, err := GetAllSessions(ctx)
	if err != nil {
		return nil, err
	}
	allSessions := allSessionsUpdate.Sessions
	for _, session := range allSessions {
		if session.SessionId == id {
			return session, nil
		}
	}
	return nil, nil
}

func GetSessionByName(ctx context.Context, name string) (*SessionType, error) {
	var session *SessionType
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT sessionid FROM session WHERE name = ?`
		sessionId := tx.GetString(query, name)
		if sessionId == "" {
			return nil
		}
		var err error
		session, err = GetSessionById(tx.Context(), sessionId)
		if err != nil {
			return err
		}
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	return session, nil
}

func InsertCloudSession(ctx context.Context, sessionName string, shareMode string, activate bool) (*ModelUpdate, error) {
	var updateRtn *ModelUpdate
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		var err error
		updateRtn, err = InsertSessionWithName(tx.Context(), sessionName, shareMode, activate)
		if err != nil {
			return err
		}
		sessionId := updateRtn.Sessions[0].SessionId
		fmt.Printf("sessionid: %v\n", sessionId)
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	return updateRtn, nil
}

// returns sessionId
// if sessionName == "", it will be generated
func InsertSessionWithName(ctx context.Context, sessionName string, shareMode string, activate bool) (*ModelUpdate, error) {
	var newScreen *ScreenType
	newSessionId := scbase.GenPromptUUID()
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		names := tx.SelectStrings(`SELECT name FROM session`)
		sessionName = fmtUniqueName(sessionName, "session-%d", len(names)+1, names)
		maxSessionIdx := tx.GetInt(`SELECT COALESCE(max(sessionidx), 0) FROM session`)
		query := `INSERT INTO session (sessionid, name, activescreenid, sessionidx, notifynum, archived, archivedts, sharemode)
                               VALUES (?,         ?,    '',             ?,          ?,         0,        0,          'local')`
		tx.Exec(query, newSessionId, sessionName, maxSessionIdx+1, 0)
		screenUpdate, err := InsertScreen(tx.Context(), newSessionId, "", true)
		if err != nil {
			return err
		}
		newScreen = screenUpdate.Screens[0]
		if activate {
			query = `UPDATE client SET activesessionid = ?`
			tx.Exec(query, newSessionId)
		}
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	session, err := GetSessionById(ctx, newSessionId)
	if err != nil {
		return nil, err
	}
	update := ModelUpdate{
		Sessions: []*SessionType{session},
		Screens:  []*ScreenType{newScreen},
	}
	if activate {
		update.ActiveSessionId = newSessionId
	}
	return &update, nil
}

func SetActiveSessionId(ctx context.Context, sessionId string) error {
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT sessionid FROM session WHERE sessionid = ?`
		if !tx.Exists(query, sessionId) {
			return fmt.Errorf("cannot switch to session, not found")
		}
		query = `UPDATE client SET activesessionid = ?`
		tx.Exec(query, sessionId)
		return nil
	})
	return txErr
}

func GetActiveSessionId(ctx context.Context) (string, error) {
	var rtnId string
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT activesessionid FROM client`
		rtnId = tx.GetString(query)
		return nil
	})
	return rtnId, txErr
}

func SetWinSize(ctx context.Context, winSize ClientWinSizeType) error {
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `UPDATE client SET winsize = ?`
		tx.Exec(query, quickJson(winSize))
		return nil
	})
	return txErr
}

func UpdateClientFeOpts(ctx context.Context, feOpts FeOptsType) error {
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `UPDATE client SET feopts = ?`
		tx.Exec(query, quickJson(feOpts))
		return nil
	})
	return txErr
}

func containsStr(strs []string, testStr string) bool {
	for _, s := range strs {
		if s == testStr {
			return true
		}
	}
	return false
}

func fmtUniqueName(name string, defaultFmtStr string, startIdx int, strs []string) string {
	var fmtStr string
	if name != "" {
		if !containsStr(strs, name) {
			return name
		}
		fmtStr = name + "-%d"
		startIdx = 2
	} else {
		fmtStr = defaultFmtStr
	}
	if strings.Index(fmtStr, "%d") == -1 {
		panic("invalid fmtStr: " + fmtStr)
	}
	for {
		testName := fmt.Sprintf(fmtStr, startIdx)
		if containsStr(strs, testName) {
			startIdx++
			continue
		}
		return testName
	}
}

func InsertScreen(ctx context.Context, sessionId string, origScreenName string, activate bool) (*ModelUpdate, error) {
	var newScreenId string
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT sessionid FROM session WHERE sessionid = ? AND NOT archived`
		if !tx.Exists(query, sessionId) {
			return fmt.Errorf("cannot create screen, no session found (or session archived)")
		}
		localRemoteId := tx.GetString(`SELECT remoteid FROM remote WHERE remotealias = ?`, LocalRemoteAlias)
		if localRemoteId == "" {
			return fmt.Errorf("cannot create screen, no local remote found")
		}
		maxScreenIdx := tx.GetInt(`SELECT COALESCE(max(screenidx), 0) FROM screen WHERE sessionid = ? AND NOT archived`, sessionId)
		var screenName string
		if origScreenName == "" {
			screenNames := tx.SelectStrings(`SELECT name FROM screen WHERE sessionid = ? AND NOT archived`, sessionId)
			screenName = fmtUniqueName("", "s%d", maxScreenIdx+1, screenNames)
		} else {
			screenName = origScreenName
		}
		newScreenId = scbase.GenPromptUUID()
		screen := &ScreenType{
			SessionId:    sessionId,
			ScreenId:     newScreenId,
			Name:         screenName,
			ScreenIdx:    int64(maxScreenIdx) + 1,
			ScreenOpts:   ScreenOptsType{},
			OwnerId:      "",
			ShareMode:    ShareModeLocal,
			CurRemote:    RemotePtrType{RemoteId: localRemoteId},
			NextLineNum:  1,
			SelectedLine: 0,
			Anchor:       ScreenAnchorType{},
			FocusType:    ScreenFocusInput,
			Archived:     false,
			ArchivedTs:   0,
		}
		query = `INSERT INTO screen ( sessionid, screenid, name, screenidx, screenopts, ownerid, sharemode, webshareopts, curremoteownerid, curremoteid, curremotename, nextlinenum, selectedline, anchor, focustype, archived, archivedts)
                             VALUES (:sessionid,:screenid,:name,:screenidx,:screenopts,:ownerid,:sharemode,:webshareopts,:curremoteownerid,:curremoteid,:curremotename,:nextlinenum,:selectedline,:anchor,:focustype,:archived,:archivedts)`
		tx.NamedExec(query, screen.ToMap())
		if activate {
			query = `UPDATE session SET activescreenid = ? WHERE sessionid = ?`
			tx.Exec(query, newScreenId, sessionId)
		}
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	newScreen, err := GetScreenById(ctx, newScreenId)
	if err != nil {
		return nil, err
	}
	update := &ModelUpdate{Screens: []*ScreenType{newScreen}}
	if activate {
		bareSession, err := GetBareSessionById(ctx, sessionId)
		if err != nil {
			return nil, txErr
		}
		update.Sessions = []*SessionType{bareSession}
	}
	return update, nil
}

func GetScreenById(ctx context.Context, screenId string) (*ScreenType, error) {
	return WithTxRtn(ctx, func(tx *TxWrap) (*ScreenType, error) {
		query := `SELECT * FROM screen WHERE screenid = ?`
		screen := GetMapGen[*ScreenType](tx, query, screenId)
		screen.Full = true
		return screen, nil
	})
}

func FindLineIdByArg(ctx context.Context, screenId string, lineArg string) (string, error) {
	var lineId string
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		lineNum, err := strconv.Atoi(lineArg)
		if err == nil {
			// valid linenum
			query := `SELECT lineid FROM line WHERE screenid = ? AND linenum = ?`
			lineId = tx.GetString(query, screenId, lineNum)
		} else if len(lineArg) == 8 {
			// prefix id string match
			query := `SELECT lineid FROM line WHERE screenid = ? AND substr(lineid, 1, 8) = ?`
			lineId = tx.GetString(query, screenId, lineArg)
		} else {
			// id match
			query := `SELECT lineid FROM line WHERE screenid = ? AND lineid = ?`
			lineId = tx.GetString(query, screenId, lineArg)
		}
		return nil
	})
	if txErr != nil {
		return "", txErr
	}
	return lineId, nil
}

func GetLineCmdByLineId(ctx context.Context, screenId string, lineId string) (*LineType, *CmdType, error) {
	return WithTxRtn3(ctx, func(tx *TxWrap) (*LineType, *CmdType, error) {
		var lineVal LineType
		query := `SELECT * FROM line WHERE screenid = ? AND lineid = ?`
		found := tx.Get(&lineVal, query, screenId, lineId)
		if !found {
			return nil, nil, nil
		}
		var cmdRtn *CmdType
		if lineVal.CmdId != "" {
			query = `SELECT * FROM cmd WHERE screenid = ? AND cmdid = ?`
			cmdRtn = GetMapGen[*CmdType](tx, query, screenId, lineVal.CmdId)
		}
		return &lineVal, cmdRtn, nil
	})
}

func GetLineCmdByCmdId(ctx context.Context, screenId string, cmdId string) (*LineType, *CmdType, error) {
	return WithTxRtn3(ctx, func(tx *TxWrap) (*LineType, *CmdType, error) {
		var lineVal LineType
		query := `SELECT * FROM line WHERE screenid = ? AND cmdid = ?`
		found := tx.Get(&lineVal, query, screenId, cmdId)
		if !found {
			return nil, nil, nil
		}
		query = `SELECT * FROM cmd WHERE screenid = ? AND cmdid = ?`
		cmdRtn := GetMapGen[*CmdType](tx, query, screenId, cmdId)
		return &lineVal, cmdRtn, nil
	})
}

func InsertLine(ctx context.Context, line *LineType, cmd *CmdType) error {
	if line == nil {
		return fmt.Errorf("line cannot be nil")
	}
	if line.LineId == "" {
		return fmt.Errorf("line must have lineid set")
	}
	if line.LineNum != 0 {
		return fmt.Errorf("line should not hage linenum set")
	}
	if cmd.ScreenId == "" {
		return fmt.Errorf("cmd should have screenid set")
	}
	return WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT screenid FROM screen WHERE screenid = ?`
		if !tx.Exists(query, line.ScreenId) {
			return fmt.Errorf("screen not found, cannot insert line[%s]", line.ScreenId)
		}
		query = `SELECT nextlinenum FROM screen WHERE screenid = ?`
		nextLineNum := tx.GetInt(query, line.ScreenId)
		line.LineNum = int64(nextLineNum)
		query = `INSERT INTO line  ( screenid, userid, lineid, ts, linenum, linenumtemp, linelocal, linetype, text, cmdid, renderer, ephemeral, contentheight, star, archived)
                            VALUES (:screenid,:userid,:lineid,:ts,:linenum,:linenumtemp,:linelocal,:linetype,:text,:cmdid,:renderer,:ephemeral,:contentheight,:star,:archived)`
		tx.NamedExec(query, line)
		query = `UPDATE screen SET nextlinenum = ? WHERE screenid = ?`
		tx.Exec(query, nextLineNum+1, line.ScreenId)
		if cmd != nil {
			cmd.OrigTermOpts = cmd.TermOpts
			cmdMap := cmd.ToMap()
			query = `
INSERT INTO cmd  ( screenid, cmdid, remoteownerid, remoteid, remotename, cmdstr, rawcmdstr, festate, statebasehash, statediffhasharr, termopts, origtermopts, status, startpk, doneinfo, rtnstate, runout, rtnbasehash, rtndiffhasharr)
          VALUES (:screenid,:cmdid,:remoteownerid,:remoteid,:remotename,:cmdstr,:rawcmdstr,:festate,:statebasehash,:statediffhasharr,:termopts,:origtermopts,:status,:startpk,:doneinfo,:rtnstate,:runout,:rtnbasehash,:rtndiffhasharr)
`
			tx.NamedExec(query, cmdMap)
		}
		return nil
	})
}

func GetCmdByScreenId(ctx context.Context, screenId string, cmdId string) (*CmdType, error) {
	var cmd *CmdType
	err := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT * FROM cmd WHERE screenid = ? AND cmdid = ?`
		cmd = GetMapGen[*CmdType](tx, query, screenId, cmdId)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return cmd, nil
}

func UpdateCmdDoneInfo(ctx context.Context, ck base.CommandKey, doneInfo *CmdDoneInfo) (*ModelUpdate, error) {
	if doneInfo == nil {
		return nil, fmt.Errorf("invalid cmddone packet")
	}
	if ck.IsEmpty() {
		return nil, fmt.Errorf("cannot update cmddoneinfo, empty ck")
	}
	var rtnCmd *CmdType
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `UPDATE cmd SET status = ?, doneinfo = ? WHERE screenid = ? AND cmdid = ?`
		tx.Exec(query, CmdStatusDone, quickJson(doneInfo), ck.GetGroupId(), ck.GetCmdId())
		var err error
		rtnCmd, err = GetCmdByScreenId(tx.Context(), ck.GetGroupId(), ck.GetCmdId())
		if err != nil {
			return err
		}
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	if rtnCmd == nil {
		return nil, fmt.Errorf("cmd data not found for ck[%s]", ck)
	}
	return &ModelUpdate{Cmd: rtnCmd}, nil
}

func UpdateCmdRtnState(ctx context.Context, ck base.CommandKey, statePtr ShellStatePtr) error {
	if ck.IsEmpty() {
		return fmt.Errorf("cannot update cmdrtnstate, empty ck")
	}
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `UPDATE cmd SET rtnbasehash = ?, rtndiffhasharr = ? WHERE screenid = ? AND cmdid = ?`
		tx.Exec(query, statePtr.BaseHash, quickJsonArr(statePtr.DiffHashArr), ck.GetGroupId(), ck.GetCmdId())
		return nil
	})
	if txErr != nil {
		return txErr
	}
	return nil
}

func AppendCmdErrorPk(ctx context.Context, errPk *packet.CmdErrorPacketType) error {
	if errPk == nil || errPk.CK.IsEmpty() {
		return fmt.Errorf("invalid cmderror packet (no ck)")
	}
	return WithTx(ctx, func(tx *TxWrap) error {
		query := `UPDATE cmd SET runout = json_insert(runout, '$[#]', ?) WHERE screenid = ? AND cmdid = ?`
		tx.Exec(query, quickJson(errPk), errPk.CK.GetGroupId(), errPk.CK.GetCmdId())
		return nil
	})
}

func ReInitFocus(ctx context.Context) error {
	return WithTx(ctx, func(tx *TxWrap) error {
		query := `UPDATE screen SET focustype = 'input'`
		tx.Exec(query)
		return nil
	})
}

func HangupAllRunningCmds(ctx context.Context) error {
	return WithTx(ctx, func(tx *TxWrap) error {
		query := `UPDATE cmd SET status = ? WHERE status = ?`
		tx.Exec(query, CmdStatusHangup, CmdStatusRunning)
		return nil
	})
}

func HangupRunningCmdsByRemoteId(ctx context.Context, remoteId string) error {
	return WithTx(ctx, func(tx *TxWrap) error {
		query := `UPDATE cmd SET status = ? WHERE status = ? AND remoteid = ?`
		tx.Exec(query, CmdStatusHangup, CmdStatusRunning, remoteId)
		return nil
	})
}

func HangupCmd(ctx context.Context, ck base.CommandKey) error {
	return WithTx(ctx, func(tx *TxWrap) error {
		query := `UPDATE cmd SET status = ? WHERE screenid = ? AND cmdid = ?`
		tx.Exec(query, CmdStatusHangup, ck.GetGroupId(), ck.GetCmdId())
		return nil
	})
}

func getNextId(ids []string, delId string) string {
	if len(ids) == 0 {
		return ""
	}
	if len(ids) == 1 {
		if ids[0] == delId {
			return ""
		}
		return ids[0]
	}
	for idx := 0; idx < len(ids); idx++ {
		if ids[idx] == delId {
			var rtnIdx int
			if idx == len(ids)-1 {
				rtnIdx = idx - 1
			} else {
				rtnIdx = idx + 1
			}
			return ids[rtnIdx]
		}
	}
	return ids[0]
}

func SwitchScreenById(ctx context.Context, sessionId string, screenId string) (*ModelUpdate, error) {
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT screenid FROM screen WHERE sessionid = ? AND screenid = ?`
		if !tx.Exists(query, sessionId, screenId) {
			return fmt.Errorf("cannot switch to screen, screen=%s does not exist in session=%s", screenId, sessionId)
		}
		query = `UPDATE session SET activescreenid = ? WHERE sessionid = ?`
		tx.Exec(query, screenId, sessionId)
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	bareSession, err := GetBareSessionById(ctx, sessionId)
	if err != nil {
		return nil, err
	}
	return &ModelUpdate{ActiveSessionId: sessionId, Sessions: []*SessionType{bareSession}}, nil
}

// screen may not exist at this point (so don't query screen table)
func cleanScreenCmds(ctx context.Context, screenId string) error {
	var removedCmds []string
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT cmdid FROM cmd WHERE screenid = ? AND cmdid NOT IN (SELECT cmdid FROM line WHERE screenid = ?)`
		removedCmds = tx.SelectStrings(query, screenId, screenId)
		query = `DELETE FROM cmd WHERE screenid = ? AND cmdid NOT IN (SELECT cmdid FROM line WHERE screenid = ?)`
		tx.Exec(query, screenId, screenId)
		return nil
	})
	if txErr != nil {
		return txErr
	}
	for _, cmdId := range removedCmds {
		DeletePtyOutFile(ctx, screenId, cmdId)
	}
	return nil
}

func ArchiveScreen(ctx context.Context, sessionId string, screenId string) (UpdatePacket, error) {
	var isActive bool
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT screenid FROM screen WHERE sessionid = ? AND screenid = ?`
		if !tx.Exists(query, sessionId, screenId) {
			return fmt.Errorf("cannot close screen (not found)")
		}
		query = `SELECT archived FROM screen WHERE sessionid = ? AND screenid = ?`
		closeVal := tx.GetBool(query, sessionId, screenId)
		if closeVal {
			return nil
		}
		query = `SELECT count(*) FROM screen WHERE sessionid = ? AND NOT archived`
		numScreens := tx.GetInt(query, sessionId)
		if numScreens <= 1 {
			return fmt.Errorf("cannot archive the last screen in a session")
		}
		query = `UPDATE screen SET archived = 1, archivedts = ?, screenidx = 0 WHERE sessionid = ? AND screenid = ?`
		tx.Exec(query, time.Now().UnixMilli(), sessionId, screenId)
		isActive = tx.Exists(`SELECT sessionid FROM session WHERE sessionid = ? AND activescreenid = ?`, sessionId, screenId)
		if isActive {
			screenIds := tx.SelectStrings(`SELECT screenid FROM screen WHERE sessionid = ? AND NOT archived ORDER BY screenidx`, sessionId)
			nextId := getNextId(screenIds, screenId)
			tx.Exec(`UPDATE session SET activescreenid = ? WHERE sessionid = ?`, nextId, sessionId)
		}
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	newScreen, err := GetScreenById(ctx, screenId)
	if err != nil {
		return nil, fmt.Errorf("cannot retrive archived screen: %w", err)
	}
	update := ModelUpdate{Screens: []*ScreenType{newScreen}}
	if isActive {
		bareSession, err := GetBareSessionById(ctx, sessionId)
		if err != nil {
			return nil, err
		}
		update.Sessions = []*SessionType{bareSession}
	}
	return update, nil
}

func UnArchiveScreen(ctx context.Context, sessionId string, screenId string) error {
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT screenid FROM screen WHERE sessionid = ? AND screenid = ? AND archived`
		if !tx.Exists(query, sessionId, screenId) {
			return fmt.Errorf("cannot re-open screen (not found or not archived)")
		}
		maxScreenIdx := tx.GetInt(`SELECT COALESCE(max(screenidx), 0) FROM screen WHERE sessionid = ? AND NOT archived`, sessionId)
		query = `UPDATE screen SET archived = 0, screenidx = ? WHERE sessionid = ? AND screenid = ?`
		tx.Exec(query, maxScreenIdx+1, sessionId, screenId)
		return nil
	})
	return txErr
}

func PurgeScreen(ctx context.Context, screenId string, sessionDel bool) (UpdatePacket, error) {
	var sessionId string
	var isActive bool
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT screenid FROM screen WHERE screenid = ?`
		if !tx.Exists(query, screenId) {
			return fmt.Errorf("cannot purge screen (not found)")
		}
		if !sessionDel {
			query = `SELECT sessionid FROM screen WHERE screenid = ?`
			sessionId = tx.GetString(query, screenId)
			if sessionId == "" {
				return fmt.Errorf("cannot purge screen (no sessionid)")
			}
			query = `SELECT count(*) FROM screen WHERE sessionid = ? AND NOT archived`
			numScreens := tx.GetInt(query, sessionId)
			if numScreens <= 1 {
				return fmt.Errorf("cannot purge the last screen in a session")
			}
			isActive = tx.Exists(`SELECT sessionid FROM session WHERE sessionid = ? AND activescreenid = ?`, sessionId, screenId)
			if isActive {
				screenIds := tx.SelectStrings(`SELECT screenid FROM screen WHERE sessionid = ? AND NOT archived ORDER BY screenidx`, sessionId)
				nextId := getNextId(screenIds, screenId)
				tx.Exec(`UPDATE session SET activescreenid = ? WHERE sessionid = ?`, nextId, sessionId)
			}
		}
		query = `DELETE FROM screen WHERE screenid = ?`
		tx.Exec(query, screenId)
		query = `DELETE FROM history WHERE screenid = ?`
		tx.Exec(query, screenId)
		query = `DELETE FROM line WHERE screenid = ?`
		tx.Exec(query, screenId)
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	go cleanScreenCmds(context.Background(), screenId)
	if sessionDel {
		return nil, nil
	}
	update := ModelUpdate{}
	update.Screens = []*ScreenType{&ScreenType{SessionId: sessionId, ScreenId: screenId, Remove: true}}
	if isActive {
		bareSession, err := GetBareSessionById(ctx, sessionId)
		if err != nil {
			return nil, err
		}
		update.Sessions = []*SessionType{bareSession}
	}
	return update, nil
}

func GetRemoteState(ctx context.Context, sessionId string, screenId string, remotePtr RemotePtrType) (*packet.ShellState, *ShellStatePtr, error) {
	ssptr, err := GetRemoteStatePtr(ctx, sessionId, screenId, remotePtr)
	if err != nil {
		return nil, nil, err
	}
	if ssptr == nil {
		return nil, nil, nil
	}
	state, err := GetFullState(ctx, *ssptr)
	if err != nil {
		return nil, nil, err
	}
	return state, ssptr, err
}

func GetRemoteStatePtr(ctx context.Context, sessionId string, screenId string, remotePtr RemotePtrType) (*ShellStatePtr, error) {
	var ssptr *ShellStatePtr
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		ri, err := GetRemoteInstance(tx.Context(), sessionId, screenId, remotePtr)
		if err != nil {
			return err
		}
		if ri == nil {
			return nil
		}
		ssptr = &ShellStatePtr{ri.StateBaseHash, ri.StateDiffHashArr}
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	return ssptr, nil
}

func validateSessionScreen(tx *TxWrap, sessionId string, screenId string) error {
	if screenId == "" {
		query := `SELECT sessionid FROM session WHERE sessionid = ?`
		if !tx.Exists(query, sessionId) {
			return fmt.Errorf("no session found")
		}
		return nil
	} else {
		query := `SELECT screenid FROM screen WHERE sessionid = ? AND screenid = ?`
		if !tx.Exists(query, sessionId, screenId) {
			return fmt.Errorf("no screen found")
		}
		return nil
	}
}

func GetRemoteInstance(ctx context.Context, sessionId string, screenId string, remotePtr RemotePtrType) (*RemoteInstance, error) {
	if remotePtr.IsSessionScope() {
		screenId = ""
	}
	var ri *RemoteInstance
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT * FROM remote_instance WHERE sessionid = ? AND screenid = ? AND remoteownerid = ? AND remoteid = ? AND name = ?`
		ri = GetMapGen[*RemoteInstance](tx, query, sessionId, screenId, remotePtr.OwnerId, remotePtr.RemoteId, remotePtr.Name)
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	return ri, nil
}

// internal function for UpdateRemoteState
func updateRIWithState(ctx context.Context, ri *RemoteInstance, stateBase *packet.ShellState, stateDiff *packet.ShellStateDiff) error {
	if stateBase != nil {
		ri.StateBaseHash = stateBase.GetHashVal(false)
		ri.StateDiffHashArr = nil
		err := StoreStateBase(ctx, stateBase)
		if err != nil {
			return err
		}
	} else if stateDiff != nil {
		ri.StateBaseHash = stateDiff.BaseHash
		ri.StateDiffHashArr = append(stateDiff.DiffHashArr, stateDiff.GetHashVal(false))
		err := StoreStateDiff(ctx, stateDiff)
		if err != nil {
			return err
		}
	}
	return nil
}

func UpdateRemoteState(ctx context.Context, sessionId string, screenId string, remotePtr RemotePtrType, feState FeStateType, stateBase *packet.ShellState, stateDiff *packet.ShellStateDiff) (*RemoteInstance, error) {
	if stateBase == nil && stateDiff == nil {
		return nil, fmt.Errorf("UpdateRemoteState, must set state or diff")
	}
	if stateBase != nil && stateDiff != nil {
		return nil, fmt.Errorf("UpdateRemoteState, cannot set state and diff")
	}
	if remotePtr.IsSessionScope() {
		screenId = ""
	}
	var ri *RemoteInstance
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		err := validateSessionScreen(tx, sessionId, screenId)
		if err != nil {
			return fmt.Errorf("cannot update remote instance state: %w", err)
		}
		query := `SELECT * FROM remote_instance WHERE sessionid = ? AND screenid = ? AND remoteownerid = ? AND remoteid = ? AND name = ?`
		ri = GetMapGen[*RemoteInstance](tx, query, sessionId, screenId, remotePtr.OwnerId, remotePtr.RemoteId, remotePtr.Name)
		if ri == nil {
			ri = &RemoteInstance{
				RIId:          scbase.GenPromptUUID(),
				Name:          remotePtr.Name,
				SessionId:     sessionId,
				ScreenId:      screenId,
				RemoteOwnerId: remotePtr.OwnerId,
				RemoteId:      remotePtr.RemoteId,
				FeState:       feState,
			}
			err = updateRIWithState(tx.Context(), ri, stateBase, stateDiff)
			if err != nil {
				return err
			}
			query = `INSERT INTO remote_instance ( riid, name, sessionid, screenid, remoteownerid, remoteid, festate, statebasehash, statediffhasharr)
                                          VALUES (:riid,:name,:sessionid,:screenid,:remoteownerid,:remoteid,:festate,:statebasehash,:statediffhasharr)`
			tx.NamedExec(query, ri.ToMap())
			return nil
		} else {
			query = `UPDATE remote_instance SET festate = ?, statebasehash = ?, statediffhasharr = ? WHERE riid = ?`
			ri.FeState = feState
			err = updateRIWithState(tx.Context(), ri, stateBase, stateDiff)
			if err != nil {
				return err
			}
			tx.Exec(query, quickJson(ri.FeState), ri.StateBaseHash, quickJsonArr(ri.StateDiffHashArr), ri.RIId)
			return nil
		}
	})
	return ri, txErr
}

func UpdateCurRemote(ctx context.Context, screenId string, remotePtr RemotePtrType) error {
	return WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT screenid FROM screen WHERE screenid = ?`
		if !tx.Exists(query, screenId) {
			return fmt.Errorf("cannot update curremote: no screen found")
		}
		query = `UPDATE screen SET curremoteownerid = ?, curremoteid = ?, curremotename = ? WHERE screenid = ?`
		tx.Exec(query, remotePtr.OwnerId, remotePtr.RemoteId, remotePtr.Name, screenId)
		return nil
	})
}

func reorderStrings(strs []string, toMove string, newIndex int) []string {
	if toMove == "" {
		return strs
	}
	var newStrs []string
	if newIndex < 0 {
		newStrs = append(newStrs, toMove)
	}
	for _, sval := range strs {
		if len(newStrs) == newIndex {
			newStrs = append(newStrs, toMove)
		}
		if sval != toMove {
			newStrs = append(newStrs, sval)
		}
	}
	if newIndex >= len(newStrs) {
		newStrs = append(newStrs, toMove)
	}
	return newStrs
}

func ReIndexSessions(ctx context.Context, sessionId string, newIndex int) error {
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT sessionid FROM session WHERE NOT archived ORDER BY sessionidx, name, sessionid`
		ids := tx.SelectStrings(query)
		if sessionId != "" {
			ids = reorderStrings(ids, sessionId, newIndex)
		}
		query = `UPDATE session SET sessionid = ? WHERE sessionid = ?`
		for idx, id := range ids {
			tx.Exec(query, id, idx+1)
		}
		return nil
	})
	return txErr
}

func SetSessionName(ctx context.Context, sessionId string, name string) error {
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT sessionid FROM session WHERE sessionid = ?`
		if !tx.Exists(query, sessionId) {
			return fmt.Errorf("session does not exist")
		}
		query = `SELECT archived FROM session WHERE sessionid = ?`
		isArchived := tx.GetBool(query, sessionId)
		if !isArchived {
			query = `SELECT sessionid FROM session WHERE name = ? AND NOT archived`
			dupSessionId := tx.GetString(query, name)
			if dupSessionId == sessionId {
				return nil
			}
			if dupSessionId != "" {
				return fmt.Errorf("invalid duplicate session name '%s'", name)
			}
		}
		query = `UPDATE session SET name = ? WHERE sessionid = ?`
		tx.Exec(query, name, sessionId)
		return nil
	})
	return txErr
}

func SetScreenName(ctx context.Context, sessionId string, screenId string, name string) error {
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT screenid FROM screen WHERE sessionid = ? AND screenid = ?`
		if !tx.Exists(query, sessionId, screenId) {
			return fmt.Errorf("screen does not exist")
		}
		query = `UPDATE screen SET name = ? WHERE sessionid = ? AND screenid = ?`
		tx.Exec(query, name, sessionId, screenId)
		return nil
	})
	return txErr
}

func ArchiveScreenLines(ctx context.Context, screenId string) (*ModelUpdate, error) {
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT screenid FROM screen WHERE screenid = ?`
		if !tx.Exists(query, screenId) {
			return fmt.Errorf("screen does not exist")
		}
		query = `UPDATE line SET archived = 1 WHERE screenid = ?`
		tx.Exec(query, screenId)
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	screenLines, err := GetScreenLinesById(ctx, screenId)
	if err != nil {
		return nil, err
	}
	return &ModelUpdate{ScreenLines: screenLines}, nil
}

func PurgeScreenLines(ctx context.Context, screenId string) (*ModelUpdate, error) {
	var lineIds []string
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT lineid FROM line WHERE screenid = ?`
		lineIds = tx.SelectStrings(query, screenId)
		query = `DELETE FROM line WHERE screenid = ?`
		tx.Exec(query, screenId)
		query = `DELETE FROM history WHERE screenid = ?`
		tx.Exec(query, screenId)
		query = `UPDATE screen SET nextlinenum = 1 WHERE screenid = ?`
		tx.Exec(query, screenId)
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	go cleanScreenCmds(context.Background(), screenId)
	screen, err := GetScreenById(ctx, screenId)
	if err != nil {
		return nil, err
	}
	screenLines, err := GetScreenLinesById(ctx, screenId)
	if err != nil {
		return nil, err
	}
	for _, lineId := range lineIds {
		line := &LineType{
			ScreenId: screenId,
			LineId:   lineId,
			Remove:   true,
		}
		screenLines.Lines = append(screenLines.Lines, line)
	}
	return &ModelUpdate{Screens: []*ScreenType{screen}, ScreenLines: screenLines}, nil
}

func GetRunningScreenCmds(ctx context.Context, screenId string) ([]*CmdType, error) {
	var rtn []*CmdType
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT * from cmd WHERE cmdid IN (SELECT cmdid FROM line WHERE screenid = ?) AND status = ?`
		rtn = SelectMapsGen[*CmdType](tx, query, screenId, CmdStatusRunning)
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	return rtn, nil
}

func UpdateCmdTermOpts(ctx context.Context, screenId string, cmdId string, termOpts TermOpts) error {
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `UPDATE cmd SET termopts = ? WHERE screenid = ? AND cmdid = ?`
		tx.Exec(query, termOpts, screenId, cmdId)
		return nil
	})
	return txErr
}

// returns riids of deleted RIs
func ScreenReset(ctx context.Context, screenId string) ([]*RemoteInstance, error) {
	return WithTxRtn(ctx, func(tx *TxWrap) ([]*RemoteInstance, error) {
		query := `SELECT sessionid FROM screen WHERE screenid = ?`
		sessionId := tx.GetString(query, screenId)
		if sessionId == "" {
			return nil, fmt.Errorf("screen does not exist")
		}
		query = `SELECT riid FROM remote_instance WHERE sessionid = ? AND screenid = ?`
		riids := tx.SelectStrings(query, sessionId, screenId)
		var delRis []*RemoteInstance
		for _, riid := range riids {
			ri := &RemoteInstance{SessionId: sessionId, ScreenId: screenId, RIId: riid, Remove: true}
			delRis = append(delRis, ri)
		}
		query = `DELETE FROM remote_instance WHERE sessionid = ? AND screenid = ?`
		tx.Exec(query, sessionId, screenId)
		return delRis, nil
	})
}

func PurgeSession(ctx context.Context, sessionId string) (UpdatePacket, error) {
	var newActiveSessionId string
	var screenIds []string
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT sessionid FROM session WHERE sessionid = ?`
		if !tx.Exists(query, sessionId) {
			return fmt.Errorf("session does not exist")
		}
		query = `SELECT screenid FROM screen WHERE sessionid = ?`
		screenIds = tx.SelectStrings(query, sessionId)
		for _, screenId := range screenIds {
			_, err := PurgeScreen(ctx, screenId, true)
			if err != nil {
				return fmt.Errorf("error purging screen[%s]: %v", screenId, err)
			}
		}
		query = `DELETE FROM session WHERE sessionid = ?`
		tx.Exec(query, sessionId)
		newActiveSessionId, _ = fixActiveSessionId(tx.Context())
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	delErr := DeleteSessionDir(ctx, sessionId)
	update := ModelUpdate{}
	if newActiveSessionId != "" {
		update.ActiveSessionId = newActiveSessionId
	}
	if delErr != nil {
		update.Info = &InfoMsgType{
			InfoMsg: fmt.Sprintf("error removing session files: %v", delErr),
		}
	}
	update.Sessions = append(update.Sessions, &SessionType{SessionId: sessionId, Remove: true})
	for _, screenId := range screenIds {
		update.Screens = append(update.Screens, &ScreenType{ScreenId: screenId, Remove: true})
	}
	return update, nil
}

func fixActiveSessionId(ctx context.Context) (string, error) {
	var newActiveSessionId string
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		curActiveSessionId := tx.GetString("SELECT activesessionid FROM client")
		query := `SELECT sessionid FROM session WHERE sessionid = ? AND NOT archived`
		if tx.Exists(query, curActiveSessionId) {
			return nil
		}
		var err error
		newActiveSessionId, err = GetFirstSessionId(tx.Context())
		if err != nil {
			return err
		}
		tx.Exec("UPDATE client SET activesessionid = ?", newActiveSessionId)
		return nil
	})
	if txErr != nil {
		return "", txErr
	}
	return newActiveSessionId, nil
}

func ArchiveSession(ctx context.Context, sessionId string) (*ModelUpdate, error) {
	if sessionId == "" {
		return nil, fmt.Errorf("invalid blank sessionid")
	}
	var newActiveSessionId string
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT sessionid FROM session WHERE sessionid = ?`
		if !tx.Exists(query, sessionId) {
			return fmt.Errorf("session does not exist")
		}
		query = `SELECT archived FROM session WHERE sessionid = ?`
		isArchived := tx.GetBool(query, sessionId)
		if isArchived {
			return nil
		}
		query = `UPDATE session SET archived = 1, archivedts = ? WHERE sessionid = ?`
		tx.Exec(query, time.Now().UnixMilli(), sessionId)
		newActiveSessionId, _ = fixActiveSessionId(tx.Context())
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	bareSession, _ := GetBareSessionById(ctx, sessionId)
	update := &ModelUpdate{}
	if bareSession != nil {
		update.Sessions = append(update.Sessions, bareSession)
	}
	if newActiveSessionId != "" {
		update.ActiveSessionId = newActiveSessionId
	}
	return update, nil
}

func UnArchiveSession(ctx context.Context, sessionId string, activate bool) (*ModelUpdate, error) {
	if sessionId == "" {
		return nil, fmt.Errorf("invalid blank sessionid")
	}
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT sessionid FROM session WHERE sessionid = ?`
		if !tx.Exists(query, sessionId) {
			return fmt.Errorf("session does not exist")
		}
		query = `SELECT archived FROM session WHERE sessionid = ?`
		isArchived := tx.GetBool(query, sessionId)
		if !isArchived {
			return nil
		}
		query = `UPDATE session SET archived = 0, archivedts = 0 WHERE sessionid = ?`
		tx.Exec(query, sessionId)
		if activate {
			query = `UPDATE client SET activesessionid = ?`
			tx.Exec(query, sessionId)
		}
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	bareSession, _ := GetBareSessionById(ctx, sessionId)
	update := &ModelUpdate{}
	if bareSession != nil {
		update.Sessions = append(update.Sessions, bareSession)
	}
	if activate {
		update.ActiveSessionId = sessionId
	}
	return update, nil
}

func GetSessionStats(ctx context.Context, sessionId string) (*SessionStatsType, error) {
	rtn := &SessionStatsType{SessionId: sessionId}
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT sessionid FROM session WHERE sessionid = ?`
		if !tx.Exists(query, sessionId) {
			return fmt.Errorf("not found")
		}
		query = `SELECT count(*) FROM screen WHERE sessionid = ? AND NOT archived`
		rtn.NumScreens = tx.GetInt(query, sessionId)
		query = `SELECT count(*) FROM screen WHERE sessionid = ? AND archived`
		rtn.NumArchivedScreens = tx.GetInt(query, sessionId)
		query = `SELECT count(*) FROM line WHERE sessionid = ?`
		rtn.NumLines = tx.GetInt(query, sessionId)
		query = `SELECT count(*) FROM cmd WHERE screenid IN (select screenid FROM screen WHERE sessionid = ?)`
		rtn.NumCmds = tx.GetInt(query, sessionId)
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	diskSize, err := SessionDiskSize(sessionId)
	if err != nil {
		return nil, err
	}
	rtn.DiskStats = diskSize
	return rtn, nil
}

const (
	RemoteField_Alias       = "alias"       // string
	RemoteField_ConnectMode = "connectmode" // string
	RemoteField_AutoInstall = "autoinstall" // bool
	RemoteField_SSHKey      = "sshkey"      // string
	RemoteField_SSHPassword = "sshpassword" // string
	RemoteField_Color       = "color"       // string
)

// editMap: alias, connectmode, autoinstall, sshkey, color, sshpassword (from constants)
func UpdateRemote(ctx context.Context, remoteId string, editMap map[string]interface{}) (*RemoteType, error) {
	var rtn *RemoteType
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT remoteid FROM remote WHERE remoteid = ?`
		if !tx.Exists(query, remoteId) {
			return fmt.Errorf("remote not found")
		}
		if alias, found := editMap[RemoteField_Alias]; found {
			query = `SELECT remoteid FROM remote WHERE remotealias = ? AND remoteid <> ?`
			if alias != "" && tx.Exists(query, alias, remoteId) {
				return fmt.Errorf("remote has duplicate alias, cannot update")
			}
			query = `UPDATE remote SET remotealias = ? WHERE remoteid = ?`
			tx.Exec(query, alias, remoteId)
		}
		if mode, found := editMap[RemoteField_ConnectMode]; found {
			query = `UPDATE remote SET connectmode = ? WHERE remoteid = ?`
			tx.Exec(query, mode, remoteId)
		}
		if autoInstall, found := editMap[RemoteField_AutoInstall]; found {
			query = `UPDATE remote SET autoinstall = ? WHERE remoteid = ?`
			tx.Exec(query, autoInstall, remoteId)
		}
		if sshKey, found := editMap[RemoteField_SSHKey]; found {
			query = `UPDATE remote SET sshopts = json_set(sshopts, '$.sshidentity', ?) WHERE remoteid = ?`
			tx.Exec(query, sshKey, remoteId)
		}
		if sshPassword, found := editMap[RemoteField_SSHPassword]; found {
			query = `UPDATE remote SET sshopts = json_set(sshopts, '$.sshpassword', ?) WHERE remoteid = ?`
			tx.Exec(query, sshPassword, remoteId)
		}
		if color, found := editMap[RemoteField_Color]; found {
			query = `UPDATE remote SET remoteopts = json_set(remoteopts, '$.color', ?) WHERE remoteid = ?`
			tx.Exec(query, color, remoteId)
		}
		var err error
		rtn, err = GetRemoteById(tx.Context(), remoteId)
		if err != nil {
			return err
		}
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	return rtn, nil
}

const (
	ScreenField_AnchorLine   = "anchorline"   // int
	ScreenField_AnchorOffset = "anchoroffset" // int
	ScreenField_SelectedLine = "selectedline" // int
	ScreenField_Focus        = "focustype"    // string
	ScreenField_TabColor     = "tabcolor"     // string
	ScreenField_PTerm        = "pterm"        // string
	ScreenField_Name         = "name"         // string
)

func UpdateScreen(ctx context.Context, screenId string, editMap map[string]interface{}) (*ScreenType, error) {
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT screenid FROM screen WHERE screenid = ?`
		if !tx.Exists(query, screenId) {
			return fmt.Errorf("screen not found")
		}
		if anchorLine, found := editMap[ScreenField_AnchorLine]; found {
			query = `UPDATE screen SET anchor = json_set(anchor, '$.anchorline', ?) WHERE screenid = ?`
			tx.Exec(query, anchorLine, screenId)
		}
		if anchorOffset, found := editMap[ScreenField_AnchorOffset]; found {
			query = `UPDATE screen SET anchor = json_set(anchor, '$.anchoroffset', ?) WHERE screenid = ?`
			tx.Exec(query, anchorOffset, screenId)
		}
		if sline, found := editMap[ScreenField_SelectedLine]; found {
			query = `UPDATE screen SET selectedline = ? WHERE screenid = ?`
			tx.Exec(query, sline, screenId)
		}
		if focusType, found := editMap[ScreenField_Focus]; found {
			query = `UPDATE screen SET focustype = ? WHERE screenid = ?`
			tx.Exec(query, focusType, screenId)
		}
		if tabColor, found := editMap[ScreenField_TabColor]; found {
			query = `UPDATE screen SET screenopts = json_set(screenopts, '$.tabcolor', ?) WHERE screenid = ?`
			tx.Exec(query, tabColor, screenId)
		}
		if pterm, found := editMap[ScreenField_PTerm]; found {
			query = `UPDATE screen SET screenopts = json_set(screenopts, '$.pterm', ?) WHERE screenid = ?`
			tx.Exec(query, pterm, screenId)
		}
		if name, found := editMap[ScreenField_Name]; found {
			query = `UPDATE screen SET name = ? WHERE screenid = ?`
			tx.Exec(query, name, screenId)
		}
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	return GetScreenById(ctx, screenId)
}

func GetLineResolveItems(ctx context.Context, screenId string) ([]ResolveItem, error) {
	var rtn []ResolveItem
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT lineid as id, linenum as num FROM line WHERE screenid = ? ORDER BY linenum`
		tx.Select(&rtn, query, screenId)
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	return rtn, nil
}

func UpdateScreenWithCmdFg(ctx context.Context, screenId string, cmdId string) (*ScreenType, error) {
	return WithTxRtn(ctx, func(tx *TxWrap) (*ScreenType, error) {
		query := `SELECT screenid
                  FROM screen s
                  WHERE s.screenid = ? AND s.focustype = 'cmd-fg'
                    AND s.selectedline IN (SELECT linenum FROM line l WHERE l.screenid = s.screenid AND l.cmdid = ?)
        `
		if !tx.Exists(query, screenId, cmdId) {
			return nil, nil
		}
		editMap := make(map[string]interface{})
		editMap[ScreenField_Focus] = ScreenFocusInput
		screen, err := UpdateScreen(tx.Context(), screenId, editMap)
		if err != nil {
			return nil, err
		}
		return screen, nil
	})
}

func StoreStateBase(ctx context.Context, state *packet.ShellState) error {
	stateBase := &StateBase{
		Version: state.Version,
		Ts:      time.Now().UnixMilli(),
	}
	stateBase.BaseHash, stateBase.Data = state.EncodeAndHash()
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT basehash FROM state_base WHERE basehash = ?`
		if tx.Exists(query, stateBase.BaseHash) {
			return nil
		}
		query = `INSERT INTO state_base (basehash, ts, version, data) VALUES (:basehash,:ts,:version,:data)`
		tx.NamedExec(query, stateBase)
		return nil
	})
	if txErr != nil {
		return txErr
	}
	return nil
}

func StoreStateDiff(ctx context.Context, diff *packet.ShellStateDiff) error {
	stateDiff := &StateDiff{
		BaseHash:    diff.BaseHash,
		Ts:          time.Now().UnixMilli(),
		DiffHashArr: diff.DiffHashArr,
	}
	stateDiff.DiffHash, stateDiff.Data = diff.EncodeAndHash()
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT basehash FROM state_base WHERE basehash = ?`
		if stateDiff.BaseHash == "" || !tx.Exists(query, stateDiff.BaseHash) {
			return fmt.Errorf("cannot store statediff, basehash:%s does not exist", stateDiff.BaseHash)
		}
		query = `SELECT diffhash FROM state_diff WHERE diffhash = ?`
		for idx, diffHash := range stateDiff.DiffHashArr {
			if !tx.Exists(query, diffHash) {
				return fmt.Errorf("cannot store statediff, diffhash[%d]:%s does not exist", idx, diffHash)
			}
		}
		if tx.Exists(query, stateDiff.DiffHash) {
			return nil
		}
		query = `INSERT INTO state_diff (diffhash, ts, basehash, diffhasharr, data) VALUES (:diffhash,:ts,:basehash,:diffhasharr,:data)`
		tx.NamedExec(query, stateDiff.ToMap())
		return nil
	})
	if txErr != nil {
		return txErr
	}
	return nil
}

// returns error when not found
func GetFullState(ctx context.Context, ssPtr ShellStatePtr) (*packet.ShellState, error) {
	var state *packet.ShellState
	if ssPtr.BaseHash == "" {
		return nil, fmt.Errorf("invalid empty basehash")
	}
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		var stateBase StateBase
		query := `SELECT * FROM state_base WHERE basehash = ?`
		found := tx.Get(&stateBase, query, ssPtr.BaseHash)
		if !found {
			return fmt.Errorf("ShellState %s not found", ssPtr.BaseHash)
		}
		state = &packet.ShellState{}
		err := state.DecodeShellState(stateBase.Data)
		if err != nil {
			return err
		}
		for idx, diffHash := range ssPtr.DiffHashArr {
			query = `SELECT * FROM state_diff WHERE diffhash = ?`
			stateDiff := GetMapGen[*StateDiff](tx, query, diffHash)
			if stateDiff == nil {
				return fmt.Errorf("ShellStateDiff %s not found", diffHash)
			}
			var ssDiff packet.ShellStateDiff
			err = ssDiff.DecodeShellStateDiff(stateDiff.Data)
			if err != nil {
				return err
			}
			newState, err := shexec.ApplyShellStateDiff(*state, ssDiff)
			if err != nil {
				return fmt.Errorf("GetFullState, diff[%d]:%s: %v", idx, diffHash, err)
			}
			state = &newState
		}
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	if state == nil {
		return nil, fmt.Errorf("ShellState not found")
	}
	return state, nil
}

func UpdateLineStar(ctx context.Context, lineId string, starVal int) error {
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `UPDATE line SET star = ? WHERE lineid = ?`
		tx.Exec(query, starVal, lineId)
		return nil
	})
	if txErr != nil {
		return txErr
	}
	return nil
}

func UpdateLineHeight(ctx context.Context, lineId string, heightVal int) error {
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `UPDATE line SET contentheight = ? WHERE lineid = ?`
		tx.Exec(query, heightVal, lineId)
		return nil
	})
	if txErr != nil {
		return txErr
	}
	return nil
}

func UpdateLineRenderer(ctx context.Context, lineId string, renderer string) error {
	return WithTx(ctx, func(tx *TxWrap) error {
		query := `UPDATE line SET renderer = ? WHERE lineid = ?`
		tx.Exec(query, renderer, lineId)
		return nil
	})
}

// can return nil, nil if line is not found
func GetLineById(ctx context.Context, screenId string, lineId string) (*LineType, error) {
	var rtn *LineType
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		var line LineType
		query := `SELECT * FROM line WHERE screenid = ? AND lineid = ?`
		found := tx.Get(&line, query, screenId, lineId)
		if found {
			rtn = &line
		}
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	return rtn, nil
}

func SetLineArchivedById(ctx context.Context, lineId string, archived bool) error {
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `UPDATE line SET archived = ? WHERE lineid = ?`
		tx.Exec(query, archived, lineId)
		return nil
	})
	return txErr
}

func purgeCmdByScreenId(ctx context.Context, screenId string, cmdId string) error {
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `DELETE FROM cmd WHERE screenid = ? AND cmdid = ?`
		tx.Exec(query, screenId, cmdId)
		return DeletePtyOutFile(tx.Context(), screenId, cmdId)
	})
	return txErr
}

func PurgeLinesByIds(ctx context.Context, screenId string, lineIds []string) error {
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		for _, lineId := range lineIds {
			query := `SELECT cmdid FROM line WHERE screenid = ? AND lineid = ?`
			cmdId := tx.GetString(query, screenId, lineId)
			query = `DELETE FROM line WHERE screenid = ? AND lineid = ?`
			tx.Exec(query, screenId, lineId)
			query = `DELETE FROM history WHERE screenid = ? AND lineid = ?`
			tx.Exec(query, screenId, lineId)
			if cmdId != "" {
				err := purgeCmdByScreenId(tx.Context(), screenId, cmdId)
				if err != nil {
					return err
				}
			}
		}
		return nil
	})
	return txErr
}

func GetRIsForScreen(ctx context.Context, sessionId string, screenId string) ([]*RemoteInstance, error) {
	var rtn []*RemoteInstance
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT * FROM remote_instance WHERE sessionid = ? AND (screenid = '' OR screenid = ?)`
		rtn = SelectMapsGen[*RemoteInstance](tx, query, sessionId, screenId)
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	return rtn, nil
}

func GetCurDayStr() string {
	now := time.Now()
	dayStr := now.Format("2006-01-02")
	return dayStr
}

func UpdateCurrentActivity(ctx context.Context, update ActivityUpdate) error {
	now := time.Now()
	dayStr := GetCurDayStr()
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		var tdata TelemetryData
		query := `SELECT tdata FROM activity WHERE day = ?`
		found := tx.Get(&tdata, query, dayStr)
		if !found {
			query = `INSERT INTO activity (day, uploaded, tdata, tzname, tzoffset, clientversion, clientarch, buildtime, osrelease)
                                   VALUES (?,   0,        ?,     ?,      ?,        ?,             ?         , ?        , ?)`
			tzName, tzOffset := now.Zone()
			if len(tzName) > MaxTzNameLen {
				tzName = tzName[0:MaxTzNameLen]
			}
			tx.Exec(query, dayStr, tdata, tzName, tzOffset, scbase.PromptVersion, scbase.ClientArch(), scbase.BuildTime, scbase.MacOSRelease())
		}
		tdata.NumCommands += update.NumCommands
		tdata.FgMinutes += update.FgMinutes
		tdata.ActiveMinutes += update.ActiveMinutes
		tdata.OpenMinutes += update.OpenMinutes
		tdata.ClickShared += update.ClickShared
		tdata.HistoryView += update.HistoryView
		tdata.BookmarksView += update.BookmarksView
		if update.NumConns > 0 {
			tdata.NumConns = update.NumConns
		}
		query = `UPDATE activity
                 SET tdata = ?,
                     clientversion = ?,
                     buildtime = ?
                 WHERE day = ?`
		tx.Exec(query, tdata, scbase.PromptVersion, scbase.BuildTime, dayStr)
		return nil
	})
	if txErr != nil {
		return txErr
	}
	return nil
}

func GetNonUploadedActivity(ctx context.Context) ([]*ActivityType, error) {
	var rtn []*ActivityType
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT * FROM activity WHERE uploaded = 0 ORDER BY day DESC LIMIT 30`
		tx.Select(&rtn, query)
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	return rtn, nil
}

// note, will not mark the current day as uploaded
func MarkActivityAsUploaded(ctx context.Context, activityArr []*ActivityType) error {
	dayStr := GetCurDayStr()
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `UPDATE activity SET uploaded = 1 WHERE day = ?`
		for _, activity := range activityArr {
			if activity.Day == dayStr {
				continue
			}
			tx.Exec(query, activity.Day)
		}
		return nil
	})
	return txErr
}

func foundInStrArr(strs []string, s string) bool {
	for _, sval := range strs {
		if s == sval {
			return true
		}
	}
	return false
}

// newPos is 0-indexed
func reorderStrs(strs []string, toMove string, newPos int) []string {
	if !foundInStrArr(strs, toMove) {
		return strs
	}
	var added bool
	rtn := make([]string, 0, len(strs))
	for _, s := range strs {
		if s == toMove {
			continue
		}
		if len(rtn) == newPos {
			added = true
			rtn = append(rtn, toMove)
		}
		rtn = append(rtn, s)
	}
	if !added {
		rtn = append(rtn, toMove)
	}
	return rtn
}

// newScreenIdx is 1-indexed
func SetScreenIdx(ctx context.Context, sessionId string, screenId string, newScreenIdx int) error {
	if newScreenIdx <= 0 {
		return fmt.Errorf("invalid screenidx/pos, must be greater than 0")
	}
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT screenid FROM screen WHERE sessionid = ? AND screenid = ? AND NOT archived`
		if !tx.Exists(query, sessionId, screenId) {
			return fmt.Errorf("invalid screen, not found (or archived)")
		}
		query = `SELECT screenid FROM screen WHERE sessionid = ? AND NOT archived ORDER BY screenidx`
		screens := tx.SelectStrings(query, sessionId)
		newScreens := reorderStrs(screens, screenId, newScreenIdx-1)
		query = `UPDATE screen SET screenidx = ? WHERE sessionid = ? AND screenid = ?`
		for idx, sid := range newScreens {
			tx.Exec(query, idx+1, sessionId, sid)
		}
		return nil
	})
	return txErr
}

func GetDBVersion(ctx context.Context) (int, error) {
	var version int
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT version FROM schema_migrations`
		version = tx.GetInt(query)
		return nil
	})
	return version, txErr
}

type bookmarkOrderType struct {
	BookmarkId string
	OrderIdx   int64
}

func GetBookmarks(ctx context.Context, tag string) ([]*BookmarkType, error) {
	var bms []*BookmarkType
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		var query string
		if tag == "" {
			query = `SELECT * FROM bookmark`
			bms = SelectMapsGen[*BookmarkType](tx, query)
		} else {
			query = `SELECT * FROM bookmark WHERE EXISTS (SELECT 1 FROM json_each(tags) WHERE value = ?)`
			bms = SelectMapsGen[*BookmarkType](tx, query, tag)
		}
		bmMap := MakeGenMap(bms)
		var orders []bookmarkOrderType
		query = `SELECT bookmarkid, orderidx FROM bookmark_order WHERE tag = ?`
		tx.Select(&orders, query, tag)
		for _, bmOrder := range orders {
			bm := bmMap[bmOrder.BookmarkId]
			if bm != nil {
				bm.OrderIdx = bmOrder.OrderIdx
			}
		}
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	return bms, nil
}

func GetBookmarkById(ctx context.Context, bookmarkId string, tag string) (*BookmarkType, error) {
	var rtn *BookmarkType
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT * FROM bookmark WHERE bookmarkid = ?`
		rtn = GetMapGen[*BookmarkType](tx, query, bookmarkId)
		if rtn == nil {
			return nil
		}
		query = `SELECT orderidx FROM bookmark_order WHERE bookmarkid = ? AND tag = ?`
		orderIdx := tx.GetInt(query, bookmarkId, tag)
		rtn.OrderIdx = int64(orderIdx)
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	return rtn, nil
}

func GetBookmarkIdByArg(ctx context.Context, bookmarkArg string) (string, error) {
	var rtnId string
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		if len(bookmarkArg) == 8 {
			query := `SELECT bookmarkid FROM bookmark WHERE bookmarkid LIKE (? || '%')`
			rtnId = tx.GetString(query, bookmarkArg)
			return nil
		}
		query := `SELECT bookmarkid FROM bookmark WHERE bookmarkid = ?`
		rtnId = tx.GetString(query, bookmarkArg)
		return nil
	})
	if txErr != nil {
		return "", txErr
	}
	return rtnId, nil
}

func GetBookmarkIdsByCmdStr(ctx context.Context, cmdStr string) ([]string, error) {
	return WithTxRtn(ctx, func(tx *TxWrap) ([]string, error) {
		query := `SELECT bookmarkid FROM bookmark WHERE cmdstr = ?`
		bmIds := tx.SelectStrings(query, cmdStr)
		return bmIds, nil
	})
}

// ignores OrderIdx field
func InsertBookmark(ctx context.Context, bm *BookmarkType) error {
	if bm == nil || bm.BookmarkId == "" {
		return fmt.Errorf("invalid empty bookmark id")
	}
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT bookmarkid FROM bookmark WHERE bookmarkid = ?`
		if tx.Exists(query, bm.BookmarkId) {
			return fmt.Errorf("bookmarkid already exists")
		}
		query = `INSERT INTO bookmark ( bookmarkid, createdts, cmdstr, alias, tags, description)
                               VALUES (:bookmarkid,:createdts,:cmdstr,:alias,:tags,:description)`
		tx.NamedExec(query, bm.ToMap())
		for _, tag := range append(bm.Tags, "") {
			query = `SELECT COALESCE(max(orderidx), 0) FROM bookmark_order WHERE tag = ?`
			maxOrder := tx.GetInt(query, tag)
			query = `INSERT INTO bookmark_order (tag, bookmarkid, orderidx) VALUES (?, ?, ?)`
			tx.Exec(query, tag, bm.BookmarkId, maxOrder+1)
		}
		return nil
	})
	return txErr
}

const (
	BookmarkField_Desc   = "desc"
	BookmarkField_CmdStr = "cmdstr"
)

func EditBookmark(ctx context.Context, bookmarkId string, editMap map[string]interface{}) error {
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT bookmarkid FROM bookmark WHERE bookmarkid = ?`
		if !tx.Exists(query, bookmarkId) {
			return fmt.Errorf("bookmark not found")
		}
		if desc, found := editMap[BookmarkField_Desc]; found {
			query = `UPDATE bookmark SET description = ? WHERE bookmarkid = ?`
			tx.Exec(query, desc, bookmarkId)
		}
		if cmdStr, found := editMap[BookmarkField_CmdStr]; found {
			query = `UPDATE bookmark SET cmdstr = ? WHERE bookmarkid = ?`
			tx.Exec(query, cmdStr, bookmarkId)
		}
		return nil
	})
	return txErr
}

func fixupBookmarkOrder(tx *TxWrap) {
	query := `
WITH new_order AS (
  SELECT tag, bookmarkid, row_number() OVER (PARTITION BY tag ORDER BY orderidx) AS newidx FROM bookmark_order
)
UPDATE bookmark_order
SET orderidx = new_order.newidx
FROM new_order
WHERE bookmark_order.tag = new_order.tag AND bookmark_order.bookmarkid = new_order.bookmarkid
`
	tx.Exec(query)
}

func DeleteBookmark(ctx context.Context, bookmarkId string) error {
	txErr := WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT bookmarkid FROM bookmark WHERE bookmarkid = ?`
		if !tx.Exists(query, bookmarkId) {
			return fmt.Errorf("bookmark not found")
		}
		query = `DELETE FROM bookmark WHERE bookmarkid = ?`
		tx.Exec(query, bookmarkId)
		query = `DELETE FROM bookmark_order WHERE bookmarkid = ?`
		tx.Exec(query, bookmarkId)
		fixupBookmarkOrder(tx)
		return nil
	})
	return txErr
}

func CreatePlaybook(ctx context.Context, name string) (*PlaybookType, error) {
	return WithTxRtn(ctx, func(tx *TxWrap) (*PlaybookType, error) {
		query := `SELECT playbookid FROM playbook WHERE name = ?`
		if tx.Exists(query, name) {
			return nil, fmt.Errorf("playbook %q already exists", name)
		}
		rtn := &PlaybookType{}
		rtn.PlaybookId = uuid.New().String()
		rtn.PlaybookName = name
		query = `INSERT INTO playbook ( playbookid, playbookname, description, entryids)
                               VALUES (:playbookid,:playbookname,:description,:entryids)`
		tx.Exec(query, rtn.ToMap())
		return rtn, nil
	})
}

func selectPlaybook(tx *TxWrap, playbookId string) *PlaybookType {
	query := `SELECT * FROM playbook where playbookid = ?`
	playbook := GetMapGen[*PlaybookType](tx, query, playbookId)
	return playbook
}

func AddPlaybookEntry(ctx context.Context, entry *PlaybookEntry) error {
	if entry.EntryId == "" {
		return fmt.Errorf("invalid entryid")
	}
	return WithTx(ctx, func(tx *TxWrap) error {
		playbook := selectPlaybook(tx, entry.PlaybookId)
		if playbook == nil {
			return fmt.Errorf("cannot add entry, playbook does not exist")
		}
		query := `SELECT entryid FROM playbook_entry WHERE entryid = ?`
		if tx.Exists(query, entry.EntryId) {
			return fmt.Errorf("cannot add entry, entryid already exists")
		}
		query = `INSERT INTO playbook_entry ( entryid, playbookid, description, alias, cmdstr, createdts, updatedts)
                                     VALUES (:entryid,:playbookid,:description,:alias,:cmdstr,:createdts,:updatedts)`
		tx.Exec(query, entry)
		playbook.EntryIds = append(playbook.EntryIds, entry.EntryId)
		query = `UPDATE playbook SET entryids = ? WHERE playbookid = ?`
		tx.Exec(query, quickJsonArr(playbook.EntryIds), entry.PlaybookId)
		return nil
	})
}

func RemovePlaybookEntry(ctx context.Context, playbookId string, entryId string) error {
	return WithTx(ctx, func(tx *TxWrap) error {
		playbook := selectPlaybook(tx, playbookId)
		if playbook == nil {
			return fmt.Errorf("cannot remove playbook entry, playbook does not exist")
		}
		query := `SELECT entryid FROM playbook_entry WHERE entryid = ?`
		if !tx.Exists(query, entryId) {
			return fmt.Errorf("cannot remove playbook entry, entry does not exist")
		}
		query = `DELETE FROM playbook_entry WHERE entryid = ?`
		tx.Exec(query, entryId)
		playbook.RemoveEntry(entryId)
		query = `UPDATE playbook SET entryids = ? WHERE playbookid = ?`
		tx.Exec(query, quickJsonArr(playbook.EntryIds), playbookId)
		return nil
	})
}

func GetPlaybookById(ctx context.Context, playbookId string) (*PlaybookType, error) {
	return WithTxRtn(ctx, func(tx *TxWrap) (*PlaybookType, error) {
		rtn := selectPlaybook(tx, playbookId)
		if rtn == nil {
			return nil, nil
		}
		query := `SELECT * FROM playbook_entry WHERE playbookid = ?`
		tx.Select(&rtn.Entries, query, playbookId)
		rtn.OrderEntries()
		return rtn, nil
	})
}

func getLineIdsFromHistoryItems(historyItems []*HistoryItemType) []string {
	var rtn []string
	for _, hitem := range historyItems {
		if hitem.LineId != "" {
			rtn = append(rtn, hitem.LineId)
		}
	}
	return rtn
}

func getCmdIdsFromHistoryItems(historyItems []*HistoryItemType) []string {
	var rtn []string
	for _, hitem := range historyItems {
		if hitem.CmdId != "" {
			rtn = append(rtn, hitem.CmdId)
		}
	}
	return rtn
}

func GetLineCmdsFromHistoryItems(ctx context.Context, historyItems []*HistoryItemType) ([]*LineType, []*CmdType, error) {
	if len(historyItems) == 0 {
		return nil, nil, nil
	}
	return WithTxRtn3(ctx, func(tx *TxWrap) ([]*LineType, []*CmdType, error) {
		var lineArr []*LineType
		query := `SELECT * FROM line WHERE lineid IN (SELECT value FROM json_each(?))`
		tx.Select(&lineArr, query, quickJsonArr(getLineIdsFromHistoryItems(historyItems)))
		query = `SELECT * FROM cmd WHERE cmdid IN (SELECT value FROM json_each(?))`
		cmdArr := SelectMapsGen[*CmdType](tx, query, quickJsonArr(getCmdIdsFromHistoryItems(historyItems)))
		return lineArr, cmdArr, nil
	})
}

func PurgeHistoryByIds(ctx context.Context, historyIds []string) ([]*HistoryItemType, error) {
	return WithTxRtn(ctx, func(tx *TxWrap) ([]*HistoryItemType, error) {
		query := `SELECT * FROM history WHERE historyid IN (SELECT value FROM json_each(?))`
		rtn := SelectMapsGen[*HistoryItemType](tx, query, quickJsonArr(historyIds))
		query = `DELETE FROM history WHERE historyid IN (SELECT value FROM json_each(?))`
		tx.Exec(query, quickJsonArr(historyIds))
		for _, hitem := range rtn {
			if hitem.LineId != "" {
				err := PurgeLinesByIds(tx.Context(), hitem.ScreenId, []string{hitem.LineId})
				if err != nil {
					return nil, err
				}
			}
		}
		return rtn, nil
	})
}

func ScreenWebShareStart(ctx context.Context, screenId string, shareOpts ScreenWebShareOpts) error {
	return WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT screenid FROM screen WHERE screenid = ?`
		if !tx.Exists(query, screenId) {
			return fmt.Errorf("screen does not exist")
		}
		shareMode := tx.GetString(`SELECT sharemode FROM screen WHERE screenid = ?`, screenId)
		if shareMode == ShareModeWeb {
			return fmt.Errorf("screen is already shared to web")
		}
		if shareMode != ShareModeLocal {
			return fmt.Errorf("screen cannot be shared, invalid current share mode %q (must be local)", shareMode)
		}
		query = `UPDATE screen SET sharemode = ?, webshareopts = ? WHERE screenid = ?`
		tx.Exec(query, ShareModeWeb, quickJson(shareOpts), screenId)
		query = `INSERT INTO screenupdates (screenid, lineid, updatetype, updatets)
                 SELECT screenid, lineid, ?, ? FROM line WHERE screenid = ? ORDER BY linenum`
		tx.Exec(query, UpdateType_LineNew, time.Now().UnixMilli(), screenId)
		return nil
	})
}

func ScreenWebShareStop(ctx context.Context, screenId string) error {
	return WithTx(ctx, func(tx *TxWrap) error {
		query := `SELECT screenid FROM screen WHERE screenid = ?`
		if !tx.Exists(query, screenId) {
			return fmt.Errorf("screen does not exist")
		}
		shareMode := tx.GetString(`SELECT sharemode FROM screen WHERE screenid = ?`, screenId)
		if shareMode != ShareModeWeb {
			return fmt.Errorf("screen is not currently shared to the web")
		}
		query = `UPDATE screen SET sharemode = ?, webshareopts = ? WHERE screenid = ?`
		tx.Exec(query, ShareModeLocal, "null", screenId)
		query = `DELETE FROM screenupdates WHERE screenid = ?`
		tx.Exec(query, screenId)
		return nil
	})
}

func isWebShare(tx *TxWrap, screenId string) bool {
	return tx.Exists(`SELECT screenid FROM screen WHERE screenid = ? AND sharemode = ?`, screenId, ShareModeWeb)
}
