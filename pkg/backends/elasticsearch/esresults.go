package elasticsearch

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"

	u "github.com/araddon/gou"
	"github.com/araddon/qlbridge/datasource"
	//"github.com/araddon/qlbridge/exec"
	"github.com/araddon/qlbridge/expr"
	"github.com/araddon/qlbridge/value"
	//"github.com/dataux/dataux/pkg/backends"
	"github.com/dataux/dataux/pkg/models"
)

var (
	_ models.ResultProvider = (*ResultReader)(nil)

	// Ensure we implement datasource.DataSource, Scanner
	_ datasource.DataSource = (*ResultReader)(nil)
	_ datasource.Scanner    = (*ResultReader)(nil)
)

// Elasticsearch ResultProvider
// - driver.Rows
// - ??  how do we get schema?
type ResultReader struct {
	exit          <-chan bool
	finalized     bool
	hasprojection bool
	cursor        int
	proj          *expr.Projection
	Docs          []u.JsonHelper
	Vals          [][]driver.Value
	Total         int
	Aggs          u.JsonHelper
	ScrollId      string
	Req           *SqlToEs
	//colnames      []string
	//Cols          []*models.ResultColumn
}

// A wrapper, allowing us to implement sql/driver Next() interface
//   which is different than qlbridge/datasource Next()
type ResultReaderNext struct {
	*ResultReader
}

func NewResultReader(req *SqlToEs) *ResultReader {
	m := &ResultReader{}
	m.Req = req
	return m
}

func (m *ResultReader) Close() error { return nil }

//func (m *ResultReader) Columns() []*models.ResultColumn { return cols }
func (m *ResultReader) buildProjection() {

	if m.hasprojection {
		return
	}
	m.hasprojection = true
	m.proj = expr.NewProjection()
	cols := m.proj.Columns
	sql := m.Req.sel
	if sql.Star {
		// Select Each field, grab fields from Table Schema
		for _, fld := range m.Req.tbl.Fields {
			cols = append(cols, expr.NewResultColumn(fld.Name, len(cols), nil, fld.Type))
		}
	} else if sql.CountStar() {
		// Count *
		cols = append(cols, expr.NewResultColumn("count", len(cols), nil, value.IntType))
	} else if len(m.Aggs) > 0 {
		if m.Req.hasSingleValue {
			for _, col := range sql.Columns {
				if col.CountStar() {
					cols = append(cols, expr.NewResultColumn(col.Key(), len(cols), col, value.IntType))
				} else {
					u.Debugf("why Aggs? %#v", col)
					cols = append(cols, expr.NewResultColumn(col.Key(), len(cols), col, value.IntType))
				}
			}
		} else if m.Req.hasMultiValue {
			// MultiValue returns are resultsets that have multiple rows for a single expression, ie top 10 terms for this field, etc
			// if len(sql.GroupBy) > 0 {
			// We store the Field Name Here
			u.Debugf("why MultiValue Aggs? %#v", m.Req)
			cols = append(cols, expr.NewResultColumn("field_name", len(cols), nil, value.StringType))
			cols = append(cols, expr.NewResultColumn("key", len(cols), nil, value.StringType)) // the value of the field
			cols = append(cols, expr.NewResultColumn("count", len(cols), nil, value.IntType))
		}
	} else {
		for _, col := range m.Req.sel.Columns {
			if fld, ok := m.Req.tbl.FieldMap[col.SourceField]; ok {
				u.Debugf("column: %#v", col)
				cols = append(cols, expr.NewResultColumn(col.SourceField, len(cols), col, fld.Type))
			} else {
				u.Debugf("Could not find: %v", col.String())
			}
		}
	}
	m.proj.Columns = cols
	u.Debugf("leaving Columns:  %v", len(m.proj.Columns))
}

/*

	// Describe the Columns etc
	Projection() *expr.Projection

*/
func (m *ResultReader) Projection() (*expr.Projection, error) {
	m.buildProjection()
	return m.proj, nil
}

func (m *ResultReader) Open(connInfo string) (datasource.DataSource, error) {
	panic("Not implemented")
	return m, nil
}

func (m *ResultReader) Schema() *models.Schema {
	return m.Req.tbl.Schema
}

func (m *ResultReader) CreateIterator(filter expr.Node) datasource.Iterator {
	return &ResultReaderNext{m}
}

// Finalize maps the Es Documents/results into
//    [][]interface{}
//
//  Normally, finalize is responsible for ensuring schema, setu
//   but in the case of elasticsearch, since it is a non-streaming
//   response, we build out values in advance
func (m *ResultReader) Finalize() error {

	m.finalized = true
	m.buildProjection()

	defer func() {
		u.Debugf("nice, finalize vals in ResultReader: %v", len(m.Vals))
	}()

	sql := m.Req.sel

	m.Vals = make([][]driver.Value, 0)

	if sql.Star {
		// ??
	} else if sql.CountStar() {
		// Count *
		vals := make([]driver.Value, 1)
		vals[0] = m.Total
		m.Vals = append(m.Vals, vals)
		return nil
	} else if len(m.Aggs) > 0 {

		if m.Req.hasMultiValue && m.Req.hasSingleValue {
			return fmt.Errorf("Must not mix single value and multi-value aggs")
		}
		if m.Req.hasSingleValue {
			vals := make([]driver.Value, len(sql.Columns))
			for i, col := range sql.Columns {
				fldName := col.Key()
				if col.Tree != nil && col.Tree.Root != nil {
					u.Debugf("col: %v", col.Tree.Root.StringAST())
				}

				if col.CountStar() {
					u.Debugf("found count star")
					vals[i] = m.Total
				} else {
					u.Debugf("looking for col: %v %v %v", fldName, m.Aggs.Get(fldName+"/value"))
					vals[i] = m.Aggs.Get(fldName + "/value")
				}

			}
			u.Debugf("write result: %v", vals)
			m.Vals = append(m.Vals, vals)
		} else if m.Req.hasMultiValue {
			// MultiValue returns are resultsets that have multiple rows for a single expression, ie top 10 terms for this field, etc

			if len(sql.GroupBy) > 0 {
				//for i, col := range sql.Columns {
				for i, _ := range sql.GroupBy {
					fldName := fmt.Sprintf("group_by_%d", i)
					u.Debugf("looking for col: %v  %v", fldName, m.Aggs.Get(fldName+"/results"))
					results := m.Aggs.Helpers(fldName + "/buckets")
					for _, result := range results {
						vals := make([]driver.Value, 3)
						vals[0] = fldName
						vals[1] = result.String("key")
						vals[2] = result.Int("doc_count")
						m.Vals = append(m.Vals, vals)
					}
					u.Debugf("missing value? %v", m.Aggs.Get(fldName))
					// by, _ := json.MarshalIndent(m.Aggs.Get(fldName), " ", " ")
					// vals[1] = by

				}
			} else {
				// MultiValue are generally aggregates
				for _, col := range sql.Columns {
					fldName := col.As
					u.Debugf("looking for col: %v  %v", fldName, m.Aggs.Get(fldName+"/results"))
					results := m.Aggs.Helpers(fldName + "/buckets")
					for _, result := range results {
						vals := make([]driver.Value, 3)
						vals[0] = fldName
						vals[1] = result.String("key")
						vals[2] = result.Int("doc_count")
						m.Vals = append(m.Vals, vals)
					}
				}
			}
		}

		//return m.conn.WriteResultset(m.conn.Status, rs)
		return nil
	}

	metaFields := map[string]byte{"_id": 1, "_type": 1, "_score": 1}

	cols := m.proj.Columns
	if len(cols) == 0 {
		u.Errorf("WTF?  no cols? %v", cols)
	}
	for _, doc := range m.Docs {
		if len(doc) > 0 {
			//by, _ := json.MarshalIndent(doc, " ", " ")
			//u.Debugf("doc: %v", string(by))

			vals := make([]driver.Value, len(m.proj.Columns))
			//for fldI, fld := range rs.Fields {
			fldI := 0

			for _, col := range cols {
				// key := "_source." + fld.FieldName
				// if _, ok := metaFields[fld.FieldName]; ok {
				// 	key = fld.FieldName
				// }
				// //u.Debugf("field: %s type=%v key='%s' %v", fld.Name, mysql.TypeString(fld.Type), key, doc.String(key))
				// switch fld.Type {
				// case mysql.MYSQL_TYPE_STRING:
				// 	vals[fldI] = doc.String(key)
				// case mysql.MYSQL_TYPE_DATETIME:
				// 	vals[fldI] = doc.String(key)
				// case mysql.MYSQL_TYPE_LONG:
				// 	vals[fldI] = doc.Int64(key)
				// case mysql.MYSQL_TYPE_FLOAT:
				// 	vals[fldI] = doc.Float64(key)
				// case mysql.MYSQL_TYPE_BLOB:
				// 	u.Debugf("blob?  %v", key)
				// 	if docVal := doc.Get(key); docVal != nil {
				// 		by, _ := json.Marshal(docVal)
				// 		vals[fldI] = string(by)
				// 	}
				// default:
				// 	u.Warnf("unrecognized type: %v", fld.String())
				// }
				key := "_source." + col.Name
				if _, ok := metaFields[col.Name]; ok {
					key = col.Name
					u.Debugf("looking for? %v in %#v", key, doc)
				}
				u.Debugf("field: %s type=%v key='%s' %v", col.Name, col.Type.String(), key, doc.String(key))
				switch col.Type {
				case value.StringType:
					vals[fldI] = doc.String(key)
				case value.TimeType:
					vals[fldI] = doc.String(key)
				case value.IntType:
					vals[fldI] = doc.Int64(key)
				case value.NumberType:
					vals[fldI] = doc.Float64(key)
				case value.ByteSliceType:
					u.Debugf("blob?  %v", key)
					if docVal := doc.Get(key); docVal != nil {
						by, _ := json.Marshal(docVal)
						vals[fldI] = string(by)
					}
				default:
					u.Warnf("unrecognized type: %v  %T", col.Name, col.Type)
				}
				fldI++
			}
			m.Vals = append(m.Vals, vals)
		}
	}

	return nil
}

// Implement sql/driver Rows Next() interface
func (m *ResultReader) Next(row []driver.Value) error {
	if m.cursor >= len(m.Vals) {
		return io.EOF
	}
	m.cursor++
	u.Debugf("ResultReader.Next():  cursor:%v  %v", m.cursor, len(m.Vals[m.cursor-1]))
	for i, val := range m.Vals[m.cursor-1] {
		row[i] = val
	}
	return nil
}

func (m *ResultReaderNext) Next() datasource.Message {
	select {
	case <-m.exit:
		return nil
	default:
		if !m.finalized {
			if err := m.Finalize(); err != nil {
				u.Errorf("Could not finalize: %v", err)
				return nil
			}
		}
		if m.cursor >= len(m.Vals) {
			return nil
		}
		m.cursor++
		u.Debugf("ResultReader.Next():  cursor:%v  %v", m.cursor, len(m.Vals[m.cursor-1]))
		return models.ValsMessage{m.Vals[m.cursor-1], uint64(m.cursor)}
	}
}
