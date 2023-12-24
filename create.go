package oracle

import (
	"bytes"

	"github.com/godoes/gorm-oracle/clauses"
	"gorm.io/gorm"
	"gorm.io/gorm/callbacks"
	"gorm.io/gorm/clause"
)

func Create(db *gorm.DB) {
	if db.Error != nil {
		return
	}

	stmt := db.Statement
	if stmt == nil {
		return
	}

	stmtSchema := stmt.Schema
	if stmtSchema == nil {
		return
	}

	if !stmt.Unscoped {
		for _, c := range stmtSchema.CreateClauses {
			stmt.AddClause(c)
		}
	}

	if stmt.SQL.Len() == 0 {
		var (
			values                  = callbacks.ConvertToCreateValues(stmt)
			onConflict, hasConflict = stmt.Clauses["ON CONFLICT"].Expression.(clause.OnConflict)
		)

		if hasConflict {
			if len(stmtSchema.PrimaryFields) > 0 {
				// are all columns in value the primary fields in schema only?
				columnsMap := map[string]bool{}
				for _, column := range values.Columns {
					columnsMap[column.Name] = true
				}

				for _, field := range stmtSchema.PrimaryFields {
					if _, ok := columnsMap[field.DBName]; !ok {
						hasConflict = false
					}
				}
			} else {
				hasConflict = false
			}
		}
		if hasConflict {
			stmt.AddClauseIfNotExists(clauses.Merge{
				Using: []clause.Interface{
					clause.Select{
						Columns: func() (columns []clause.Column) {
							// HACK: I can not come up with a better alternative for now
							// I want to add a value to the list of variable and then capture the bind variable position as well
							columns = values.Columns
							for i, column := range columns {
								buf := bytes.NewBufferString("")
								stmt.Vars = append(stmt.Vars, values.Values[0][i])
								stmt.BindVarTo(buf, stmt, nil)

								column.Alias = column.Name
								// then the captured bind var will be the name
								column.Name = buf.String()
								columns[i] = column
							}
							return
						}(),
					},
					clause.From{
						Tables: []clause.Table{{Name: db.Dialector.(Dialector).DummyTableName()}},
					},
				},
				On: func() (onExpr []clause.Expression) {
					onExpr = make([]clause.Expression, len(stmtSchema.PrimaryFields))
					for i, field := range stmtSchema.PrimaryFields {
						onExpr[i] = clause.Eq{
							Column: clause.Column{Table: stmt.Schema.Table, Name: field.DBName},
							Value:  clause.Column{Table: clauses.MergeDefaultExcludeName(), Name: field.DBName},
						}
					}
					return
				}(),
			})
			stmt.AddClauseIfNotExists(clauses.WhenMatched{Set: onConflict.DoUpdates})
			stmt.AddClauseIfNotExists(clauses.WhenNotMatched{Values: values})

			stmt.Build("MERGE", "WHEN MATCHED", "WHEN NOT MATCHED")
		} else {
			stmt.AddClauseIfNotExists(clause.Insert{Table: clause.Table{Name: stmt.Schema.Table}})
			stmt.AddClause(clause.Values{Columns: values.Columns, Values: [][]interface{}{values.Values[0]}})
			stmt.Build("INSERT", "VALUES")
		}

		if !db.DryRun && db.Error == nil {
			for _, value := range values.Values {
				// HACK: replace values one by one, assuming its value layout will be the same all the time, i.e. aligned
				for idx, val := range value {
					switch v := val.(type) {
					case bool:
						if v {
							val = 1
						} else {
							val = 0
						}
					default:
						val = convertCustomType(val)
					}

					stmt.Vars[idx] = val
				}
				// and then we insert each row one by one then put the returning values back (i.e. last return id => smart insert)
				// we keep track of the index so that the sub-reflected value is also correct

				// BIG BUG: what if any of the transactions failed? some result might already be inserted that oracle is so
				// sneaky that some transaction inserts will exceed the buffer and so will be pushed at unknown point,
				// resulting in dangling row entries, so we might need to delete them if an error happens

				result, err := stmt.ConnPool.ExecContext(stmt.Context, stmt.SQL.String(), stmt.Vars...)
				if db.AddError(err) == nil {
					// success
					db.RowsAffected, _ = result.RowsAffected()
				}
			}
		}
	}
}
