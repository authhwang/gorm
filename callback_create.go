package gorm

import (
	"fmt"
	"strings"
)

// Define callbacks for creating
// 针对简单的Create，遍历Craetes数组里面的方法
/*
	1.创建sql包的transaction
	2.调用每个模型下的自定义生命周期方法BeforeSave, BeforeCreate(将结构体转为reflect.Value类型，调用MethodByName方法获取methodValue,然后用methodValue.Interface().(type)获取其方法传入参数和返回值进行不同方法类型调用)
	3.检查是否有是属于BelongsTo关系的模型，有则先预插入保存处理
	4.更新Created_at 和 updated_at时间戳
	5.遍历所有field，获取所有变量值，合成insert sql语句，执行sql包的exec
	6.检查是否有属于hasOne、hasMany、manyToMany的关系，进行保存处理
	7.调用每个模型下的自定义生命周期方法AfterSave, AfterCreate
	8.查看是否有错误产生，有则调用rollback，无则commit
*/

func init() {
	DefaultCallback.Create().Register("gorm:begin_transaction", beginTransactionCallback)
	DefaultCallback.Create().Register("gorm:before_create", beforeCreateCallback)
	DefaultCallback.Create().Register("gorm:save_before_associations", saveBeforeAssociationsCallback)
	DefaultCallback.Create().Register("gorm:update_time_stamp", updateTimeStampForCreateCallback)
	DefaultCallback.Create().Register("gorm:create", createCallback)
	DefaultCallback.Create().Register("gorm:force_reload_after_create", forceReloadAfterCreateCallback)
	DefaultCallback.Create().Register("gorm:save_after_associations", saveAfterAssociationsCallback)
	DefaultCallback.Create().Register("gorm:after_create", afterCreateCallback)
	DefaultCallback.Create().Register("gorm:commit_or_rollback_transaction", commitOrRollbackTransactionCallback)
}

// beforeCreateCallback will invoke `BeforeSave`, `BeforeCreate` method before creating
func beforeCreateCallback(scope *Scope) {
	if !scope.HasError() {
		scope.CallMethod("BeforeSave")
	}
	if !scope.HasError() {
		scope.CallMethod("BeforeCreate")
	}
}

// updateTimeStampForCreateCallback will set `CreatedAt`, `UpdatedAt` when creating
func updateTimeStampForCreateCallback(scope *Scope) {
	if !scope.HasError() {
		now := scope.db.nowFunc()

		if createdAtField, ok := scope.FieldByName("CreatedAt"); ok {
			if createdAtField.IsBlank {
				createdAtField.Set(now)
			}
		}

		if updatedAtField, ok := scope.FieldByName("UpdatedAt"); ok {
			if updatedAtField.IsBlank {
				updatedAtField.Set(now)
			}
		}
	}
}

// createCallback the callback used to insert data into database
func createCallback(scope *Scope) {
	if !scope.HasError() {
		defer scope.trace(scope.db.nowFunc())

		var (
			columns, placeholders        []string
			blankColumnsWithDefaultValue []string
		)

		//遍历所有fields，查看有多少需要传入的列
		//callback_create.go createCallback() columns & placeholoder ==  [`created_at` `updated_at` `deleted_at` `name`] [$$$ $$$ $$$ $$$]
		for _, field := range scope.Fields() {
			if scope.changeableField(field) {
				if field.IsNormal && !field.IsIgnored {
					if field.IsBlank && field.HasDefaultValue {
						blankColumnsWithDefaultValue = append(blankColumnsWithDefaultValue, scope.Quote(field.DBName))
						scope.InstanceSet("gorm:blank_columns_with_default_value", blankColumnsWithDefaultValue)
					} else if !field.IsPrimaryKey || !field.IsBlank {
						columns = append(columns, scope.Quote(field.DBName))
						placeholders = append(placeholders, scope.AddToVars(field.Field.Interface()))
					}
				} else if field.Relationship != nil && field.Relationship.Kind == "belongs_to" {
					for _, foreignKey := range field.Relationship.ForeignDBNames {
						if foreignField, ok := scope.FieldByName(foreignKey); ok && !scope.changeableField(foreignField) {
							columns = append(columns, scope.Quote(foreignField.DBName))
							placeholders = append(placeholders, scope.AddToVars(foreignField.Field.Interface()))
						}
					}
				}
			}
		}

		var (
			returningColumn = "*"
			quotedTableName = scope.QuotedTableName()
			primaryField    = scope.PrimaryField()
			extraOption     string
			insertModifier  string
		)

		if str, ok := scope.Get("gorm:insert_option"); ok {
			extraOption = fmt.Sprint(str)
		}
		if str, ok := scope.Get("gorm:insert_modifier"); ok {
			insertModifier = strings.ToUpper(fmt.Sprint(str))
			if insertModifier == "INTO" {
				insertModifier = ""
			}
		}

		if primaryField != nil {
			returningColumn = scope.Quote(primaryField.DBName)
		}

		lastInsertIDReturningSuffix := scope.Dialect().LastInsertIDReturningSuffix(quotedTableName, returningColumn)
		lastInsertIDOutputInterstitial := scope.Dialect().LastInsertIDOutputInterstitial(quotedTableName, returningColumn, columns)

		if len(columns) == 0 {
			scope.Raw(fmt.Sprintf(
				"INSERT%v INTO %v %v%v%v",
				addExtraSpaceIfExist(insertModifier),
				quotedTableName,
				scope.Dialect().DefaultValueStr(),
				addExtraSpaceIfExist(extraOption),
				addExtraSpaceIfExist(lastInsertIDReturningSuffix),
			))
		} else {
			scope.Raw(fmt.Sprintf(
				"INSERT%v INTO %v (%v)%v VALUES (%v)%v%v",
				addExtraSpaceIfExist(insertModifier),
				scope.QuotedTableName(),
				strings.Join(columns, ","),
				addExtraSpaceIfExist(lastInsertIDOutputInterstitial),
				strings.Join(placeholders, ","),
				addExtraSpaceIfExist(extraOption),
				addExtraSpaceIfExist(lastInsertIDReturningSuffix),
			))
		}

		// execute create sql: no primaryField
		if primaryField == nil {
			if result, err := scope.SQLDB().Exec(scope.SQL, scope.SQLVars...); scope.Err(err) == nil {
				// set rows affected count
				scope.db.RowsAffected, _ = result.RowsAffected()

				// set primary value to primary field
				//设置主键值
				if primaryField != nil && primaryField.IsBlank {
					if primaryValue, err := result.LastInsertId(); scope.Err(err) == nil {
						scope.Err(primaryField.Set(primaryValue))
					}
				}
			}
			return
		}

		// execute create sql: lastInsertID implemention for majority of dialects
		if lastInsertIDReturningSuffix == "" && lastInsertIDOutputInterstitial == "" {
			if result, err := scope.SQLDB().Exec(scope.SQL, scope.SQLVars...); scope.Err(err) == nil {
				// set rows affected count
				scope.db.RowsAffected, _ = result.RowsAffected()

				// set primary value to primary field
				if primaryField != nil && primaryField.IsBlank {
					if primaryValue, err := result.LastInsertId(); scope.Err(err) == nil {
						scope.Err(primaryField.Set(primaryValue))
					}
				}
			}
			return
		}

		// execute create sql: dialects with additional lastInsertID requirements (currently postgres & mssql)
		if primaryField.Field.CanAddr() {
			if err := scope.SQLDB().QueryRow(scope.SQL, scope.SQLVars...).Scan(primaryField.Field.Addr().Interface()); scope.Err(err) == nil {
				primaryField.IsBlank = false
				scope.db.RowsAffected = 1
			}
		} else {
			scope.Err(ErrUnaddressable)
		}
		return
	}
}

// forceReloadAfterCreateCallback will reload columns that having default value, and set it back to current object
func forceReloadAfterCreateCallback(scope *Scope) {
	if blankColumnsWithDefaultValue, ok := scope.InstanceGet("gorm:blank_columns_with_default_value"); ok {
		db := scope.DB().New().Table(scope.TableName()).Select(blankColumnsWithDefaultValue.([]string))
		for _, field := range scope.Fields() {
			if field.IsPrimaryKey && !field.IsBlank {
				//查找其他不为空字段，并用where user =  1这样的方式保存
				db = db.Where(fmt.Sprintf("%v = ?", field.DBName), field.Field.Interface())
			}
		}
		//将通过sql语句查找的其他默认都注入到结构体变量中
		db.Scan(scope.Value)
	}
}

// afterCreateCallback will invoke `AfterCreate`, `AfterSave` method after creating
func afterCreateCallback(scope *Scope) {
	if !scope.HasError() {
		scope.CallMethod("AfterCreate")
	}
	if !scope.HasError() {
		scope.CallMethod("AfterSave")
	}
}
