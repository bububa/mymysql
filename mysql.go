package mymy

import (
    "os"
    "io"
    "net"
    "bufio"
    "sync"
    "fmt"
    "reflect"
)

type ServerInfo struct {
    prot_ver byte
    serv_ver string
    thr_id   uint32
    scramble []byte
    caps     uint16
    lang     byte
}

// MySQL connection handler
type MySQL struct {
    proto string // Network protocol
    laddr string // Local address
    raddr string // Remote (server) address

    user   string // MySQL username
    passwd string // MySQL password
    dbname string // Database name

    conn net.Conn      // MySQL connection
    rd   *bufio.Reader
    wr   *bufio.Writer

    info ServerInfo // MySQL server information
    seq  byte       // MySQL sequence number

    mutex         *sync.Mutex // For concurency
    unreaded_rows bool
    stmt_map      map[uint32]*Statement // For reprepare during reconnect

    // Maximum packet size that client can accept from server.
    // Default 16*1024*1024-1. You may change it before connect.
    MaxPktSize int

    // Debug logging. You may change it at any time.
    Debug bool

    // Maximum reconnect retries - for XxxAC methods. Default is 5 which
    // means 1+2+3+4+5 = 15 seconds before return an error.
    MaxRetries int
}

// Create new MySQL handler. The first three arguments are passed to net.Bind
// for create connection. user and passwd are for authentication. Optional db
// is database name (you may not specifi it and use Use() method later).
func New(proto, laddr, raddr, user, passwd string, db ...string) (my *MySQL) {
    my = &MySQL{
        proto:      proto,
        laddr:      laddr,
        raddr:      raddr,
        user:       user,
        passwd:     passwd,
        mutex:      new(sync.Mutex),
        stmt_map:   make(map[uint32]*Statement),
        MaxPktSize: 16*1024*1024-1,
        MaxRetries: 7,
    }
    if len(db) == 1 {
        my.dbname = db[0]
    } else if len(db) > 1 {
        panic("mymy.New: too many arguments")
    }
    return
}

// Thread unsafe connect
func (my *MySQL) connect() (err os.Error) {
    defer catchOsError(&err)

    // Make connection
    my.conn, err = net.Dial(my.proto, my.laddr, my.raddr)
    if err != nil {
        return
    }
    my.rd = bufio.NewReader(my.conn)
    my.wr = bufio.NewWriter(my.conn)

    // Initialisation
    my.init()
    my.auth()
    my.getResult(nil)

    return
}

// Establishes a connection with MySQL server version 4.1 or later.
func (my *MySQL) Connect() (err os.Error) {
    if my.conn != nil {
        return ALREDY_CONN_ERROR
    }
    defer my.unlock()
    my.lock()

    return my.connect()
}

// Thread unsafe close
func (my *MySQL) close_conn() (err os.Error) {
    defer catchOsError(&err)

    // Close the connection
    my.sendCmd(_COM_QUIT)
    err = my.conn.Close()
    my.conn = nil // Mark that we disconnect

    return
}

// Close connection to the server
func (my *MySQL) Close() (err os.Error) {
    if my.conn == nil {
        return NOT_CONN_ERROR
    }
    if my.unreaded_rows {
        return UNREADED_ROWS_ERROR
    }
    defer my.unlock()
    my.lock()

    return my.close_conn()
}

// Close and reopen connection in one, thread safe operation.
// Ignore unreaded rows, reprepare all prepared statements.
func (my *MySQL) Reconnect() (err os.Error) {
    defer my.unlock()
    my.lock()

    if my.conn != nil {
        // Close connection, ignore all errors
        my.close_conn()
    }
    // Reopen the connection.
    if err = my.connect(); err != nil {
        return
    }
    // Reprepare all prepared statements
    var (
        new_stmt *Statement
        new_map = make(map[uint32]*Statement)
    )
    for _, stmt := range my.stmt_map {
        new_stmt, err = my.prepare(stmt.sql)
        if err != nil {
            return
        }
        // Assume that fields set in new_stmt by prepare() are indentical to
        // corresponding fields in stmt. Why they can be different?
        stmt.id = new_stmt.id
        stmt.rebind = true
        new_map[stmt.id] = stmt
    }
    // Replace the stmt_map
    my.stmt_map = new_map

    return
}

// Change database
func (my *MySQL) Use(dbname string) (err os.Error) {
    if my.conn == nil {
        return NOT_CONN_ERROR
    }
    if my.unreaded_rows {
        return UNREADED_ROWS_ERROR
    }
    defer my.unlock()
    defer catchOsError(&err)
    my.lock()

    // Send command
    my.sendCmd(_COM_INIT_DB, dbname)
    // Get server response
    my.getResult(nil)
    // Save new database name
    my.dbname = dbname

    return
}

func (my *MySQL) getResponse() (res *Result) {
    res, ok := my.getResult(nil).(*Result)
    if !ok {
        panic(BAD_RESULT_ERROR)
    }
    if res.FieldCount == 0 {
        // This query was ended (OK result)
        my.unlock()
    } else {
        // This query can return rows
        res.db = my
        my.unreaded_rows = true
    }
    return
}

// Start new query session.
//
// command can be SQL query (string) or a prepared statement (*Statement).
//
// If the command is a string and you specify the parameters, the SQL string
// will be a result of fmt.Sprintf(command, params...).
//
// If the command is a prepared statement, params will be binded to this
// statement before execution.
//
// You must get all result rows (if they exists) before next query.
func (my *MySQL) Start(command interface{}, params ...interface{}) (
        res *Result, err os.Error) {

    // Check type of command
    switch cmd := command.(type) {
    case *Statement:
        // Prepared statement
        cmd.BindParams(params...)
        return cmd.Run()

    case string:
        // Text SQL
        if my.conn == nil {
            return nil, NOT_CONN_ERROR
        }
        if my.unreaded_rows {
            return nil, UNREADED_ROWS_ERROR
        }
        defer my.unlockIfError(&err)
        defer catchOsError(&err)
        my.lock()

        // Send query
        if len(params) == 0 {
            my.sendCmd(_COM_QUERY, cmd)
        } else {
            my.sendCmd(_COM_QUERY, fmt.Sprintf(cmd, params...))
        }

        // Get command response
        res = my.getResponse()
        return
    }
    return nil, BAD_COMMAND_ERROR
}

// Get the data row from a server. This method reads one row of result directly
// from network connection (without rows buffering on client side).
func (res *Result) GetRow() (row *Row, err os.Error) {
    if res.FieldCount == 0 {
        // There is no fields in result (OK result)
        return
    }
    defer res.db.unlockIfError(&err)
    defer catchOsError(&err)

    switch result := res.db.getResult(res).(type) {
    case *Row:
        // Row of data
        row = result

    case *Result:
        // EOF result
        if res.Status & _SERVER_MORE_RESULTS_EXISTS == 0 {
            // There is no more results
            res.db.unlock()
        }
        res.db.unreaded_rows = false

    default:
        err = BAD_RESULT_ERROR
    }
    return
}

// Return the next result or nil if no more resuts exists.
func (res *Result) NextResult() (next *Result, err os.Error) {
    if res.Status & _SERVER_MORE_RESULTS_EXISTS == 0 {
        return
    }
    next = res.db.getResponse()
    return
}

// Read all unreaded rows and discard them. All the rows must be read before
// next query or other command.
func (res *Result) End() (err os.Error) {
    for err == nil && res.db.unreaded_rows {
        _, err = res.GetRow()
    }
    return
}

// This call Start and next call GetRow once or more times. It read all rows
// from connection and returns they as a slice.
func (my *MySQL) Query(command interface{}, params ...interface{}) (
        rows []*Row, res *Result, err os.Error) {

    res, err = my.Start(command, params...)
    if err != nil {
        return
    }
    // Read rows
    var row *Row
    for {
        row, err = res.GetRow()
        if err != nil || row == nil {
            break
        }
        rows = append(rows, row)
    }
    return
}

// Send MySQL PING to the server.
func (my *MySQL) Ping() (err os.Error) {
    if my.conn == nil {
        return NOT_CONN_ERROR
    }
    if my.unreaded_rows {
        return UNREADED_ROWS_ERROR
    }
    defer my.unlock()
    defer catchOsError(&err)
    my.lock()

    // Send command
    my.sendCmd(_COM_PING)
    // Get server response
    my.getResult(nil)

    return
}

func (my *MySQL) prepare(sql string) (stmt *Statement, err os.Error) {
    defer catchOsError(&err)

    // Send command
    my.sendCmd(_COM_STMT_PREPARE, sql)
    // Get server response
    stmt, ok := my.getPrepareResult(nil).(*Statement)
    if !ok {
        return nil, BAD_RESULT_ERROR
    }
    if len(stmt.params) > 0 {
        // Get param fields
        my.getPrepareResult(stmt)
    }
    if len(stmt.Fields) > 0 {
        // Get column fields
        my.getPrepareResult(stmt)
    }
    return
}

// Prepare server side statement. Return statement handler.
func (my *MySQL) Prepare(sql string) (stmt *Statement, err os.Error) {
    if my.conn == nil {
        return nil, NOT_CONN_ERROR
    }
    if my.unreaded_rows {
        return nil, UNREADED_ROWS_ERROR
    }
    defer my.unlock()
    my.lock()

    stmt, err = my.prepare(sql)
    if err != nil {
        return
    }
    // Connect statement with database handler
    stmt.db = my
    my.stmt_map[stmt.id] = stmt
    // Save SQL for reconnect
    stmt.sql = sql

    return
}

// Bind input data for the parameter markers in the SQL statement that was
// passed to Prepare.
// 
// params may be a parameter list (slice), a struct or a pointer to the struct.
// A struct field can by value or pointer to value. A parameter (slice element)
// can be value, pointer to value or pointer to pointer to value.
// Values may be of the folowind types: intXX, uintXX, floatXX, []byte, Blob,
// string, Datetime, Timestamp, Raw.
func (stmt *Statement) BindParams(params ...interface{}) {
    stmt.rebind = true

    // Check for struct
    if len(params) == 1 {
        pval := reflect.NewValue(params[0])
        // Dereference pointer
        if vv, ok := pval.(*reflect.PtrValue); ok {
            pval = vv.Elem()
        }
        val, ok := pval.(*reflect.StructValue)
        if ok && val.Type() != reflectDatetimeType &&
                val.Type() != reflectTimestampType {
            // We have struct to bind
            if val.NumField() != stmt.ParamCount {
                panic(BIND_COUNT_ERROR)
            }
            for ii := 0; ii < stmt.ParamCount; ii ++ {
                stmt.params[ii] = bindValue(val.Field(ii))
            }
            return
        }

    }

    if len(params) != stmt.ParamCount {
        panic(BIND_COUNT_ERROR)
    }
    for ii, par := range params {
        pval := reflect.NewValue(par)
        // Dereference pointer
        if vv, ok := pval.(*reflect.PtrValue); ok {
            pval = vv.Elem()
        }
        stmt.params[ii] = bindValue(pval)
    }
}

// Execute prepared statement. If statement requires parameters you must
// bind them first. After this command you may use GetRow to retrieve data.
func (stmt *Statement) Run() (res *Result, err os.Error) {
    if stmt.db.conn == nil {
        return nil, NOT_CONN_ERROR
    }
    if stmt.db.unreaded_rows {
        return nil, UNREADED_ROWS_ERROR
    }
    defer stmt.db.unlockIfError(&err)
    defer catchOsError(&err)
    stmt.db.lock()

    // Send EXEC command with binded parameters
    stmt.sendCmdExec()
    // Get response
    res = stmt.db.getResponse()
    res.binary = true
    return
}

// This call Run and next call GetRow once or more times. It read all rows
// from connection and returns they as a slice.
func (stmt *Statement) Exec(command interface{}, params ...interface{}) (
        rows []*Row, res *Result, err os.Error) {

    res, err = stmt.Run()
    if err != nil {
        return
    }
    // Read rows
    var row *Row
    for {
        row, err = res.GetRow()
        if err != nil || row == nil {
            break
        }
        rows = append(rows, row)
    }
    return
}

// Destroy statement on server side. Client side handler is invalid after this
// command.
func (stmt *Statement) Delete() (err os.Error) {
    if stmt.db.conn == nil {
        return NOT_CONN_ERROR
    }
    if stmt.db.unreaded_rows {
        return UNREADED_ROWS_ERROR
    }
    defer stmt.db.unlock()
    defer catchOsError(&err)
    stmt.db.lock()

    // Send command
    stmt.db.sendCmd(_COM_STMT_CLOSE, stmt.id)
    // Delete statement from stmt_map
    stmt.db.stmt_map[stmt.id] = nil, false
    // Invalidate handler
    *stmt = Statement{}
    return
}

// Resets a prepared statement on server: data sent to the server, unbuffered
// result sets and current errors.
func (stmt *Statement) Reset() (err os.Error) {
    if stmt.db.conn == nil {
        return NOT_CONN_ERROR
    }
    if stmt.db.unreaded_rows {
        return UNREADED_ROWS_ERROR
    }
    defer stmt.db.unlock()
    defer catchOsError(&err)
    stmt.db.lock()

    // Send command
    stmt.db.sendCmd(_COM_STMT_RESET, stmt.id)
    // Get result
    stmt.db.getResult(nil)
    // Next exec must send type information.
    stmt.rebind = true
    return
}

// Send long data to MySQL server in chunks.
// You can call this method after Bind and before Run/Execute. It can be called
// multiple times for one parameter to send TEXT or BLOB data in chunks.
//
// pnum     - Parameter number to associate the data with.
//
// data     - Data source string, []byte or io.Reader.
//
// pkt_size - It must be must be greater than 6 and less or equal to MySQL
// max_allowed_packet variable. You can obtain value of this variable
// using such query: SHOW variables WHERE Variable_name = 'max_allowed_packet'
// If data source is io.Reader then (pkt_size - 6) is size of a buffer that
// will be allocated for reading. 
//
// If you have data source of type string or []byte in one piece you may
// properly set pkt_size and call this method once. If you have data in
// multiple pieces you can call this method multiple times. If data source is
// io.Reader you should properly set pkt_size. Data will be readed from
// io.Reader and send in pieces to the server until EOF.
func (stmt *Statement) SendLongData(pnum int, data interface{}, pkt_size int) (
        err os.Error) {

    if stmt.db.conn == nil {
        return NOT_CONN_ERROR
    }
    if stmt.db.unreaded_rows {
        return UNREADED_ROWS_ERROR
    }
    if pnum < 0 || pnum >= stmt.ParamCount {
        return WRONG_PARAM_NUM_ERROR
    }
    if pkt_size -= 6; pkt_size < 0 {
        return SMALL_PKT_SIZE_ERROR
    }
    defer stmt.db.unlock()
    defer catchOsError(&err)
    stmt.db.lock()

    switch dd := data.(type) {
    case io.Reader:
        buf := make([]byte, pkt_size)
        for {
            nn, ee := io.ReadFull(dd, buf)
            if ee == os.EOF {
                return
            }
            if nn != 0 {
                stmt.db.sendCmd(
                    _COM_STMT_SEND_LONG_DATA,
                    stmt.id, uint16(pnum), buf[0:nn],
                )
            }
            if ee == io.ErrUnexpectedEOF {
                return
            } else if ee != nil {
                return ee
            }
        }

    case []byte:
        for len(dd) > pkt_size {
            stmt.db.sendCmd(
                _COM_STMT_SEND_LONG_DATA,
                stmt.id, uint16(pnum), dd[0:pkt_size],
            )
            dd = dd[pkt_size:]
        }
        stmt.db.sendCmd(_COM_STMT_SEND_LONG_DATA, stmt.id, uint16(pnum), dd)
        return

    case string:
        for len(dd) > pkt_size {
            stmt.db.sendCmd(
                _COM_STMT_SEND_LONG_DATA,
                stmt.id, uint16(pnum), dd[0:pkt_size],
            )
            dd = dd[pkt_size:]
        }
        stmt.db.sendCmd(_COM_STMT_SEND_LONG_DATA, stmt.id, uint16(pnum), dd)
        return
    }
    return UNK_DATA_TYPE_ERROR
}
