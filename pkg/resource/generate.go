package resource

import (
	"sync"

	"github.com/hashicorp/go-hclog"
	"github.com/sev-2/raiden"
	"github.com/sev-2/raiden/pkg/generator"
	"github.com/sev-2/raiden/pkg/logger"
	"github.com/sev-2/raiden/pkg/state"
	"github.com/sev-2/raiden/pkg/supabase/objects"
	"github.com/sev-2/raiden/pkg/utils"
)

var GenerateLogger hclog.Logger = logger.HcLog().Named("import.generate")

// The `generateResource` function generates various resources such as table, roles, policy and etc
// also generate framework resource like controller, route, main function and etc
func generateResource(config *raiden.Config, importState *ResourceState, projectPath string, resource *Resource) error {
	if err := generator.CreateInternalFolder(projectPath); err != nil {
		return err
	}

	wg, errChan, stateChan := sync.WaitGroup{}, make(chan error), make(chan any)
	doneListen := ListenImportResource(importState, stateChan)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if len(resource.Tables) > 0 {
			tableInputs := buildGenerateModelInputs(resource.Tables, resource.Policies)
			GenerateLogger.Info("start - generate tables")
			captureFunc := ImportDecorateFunc(tableInputs, func(item *generator.GenerateModelInput, input generator.GenerateInput) bool {
				if i, ok := input.BindData.(generator.GenerateModelData); ok {
					if i.StructName == utils.SnakeCaseToPascalCase(item.Table.Name) {
						return true
					}
				}
				return false
			}, stateChan)

			if err := generator.GenerateModels(projectPath, tableInputs, captureFunc); err != nil {
				errChan <- err
			}
			GenerateLogger.Info("finish - generate tables")
		}

		// generate all roles from cloud / pg-meta
		if len(resource.Roles) > 0 {
			GenerateLogger.Info("start - generate roles")
			captureFunc := ImportDecorateFunc(resource.Roles, func(item objects.Role, input generator.GenerateInput) bool {
				if i, ok := input.BindData.(generator.GenerateRoleData); ok {
					if i.Name == item.Name {
						return true
					}
				}
				return false
			}, stateChan)

			if err := generator.GenerateRoles(projectPath, resource.Roles, captureFunc); err != nil {
				errChan <- err
			}
			GenerateLogger.Info("finish - generate roles")
		}

		if len(resource.Functions) > 0 {
			GenerateLogger.Info("start - generate functions")
			captureFunc := ImportDecorateFunc(resource.Functions, func(item objects.Function, input generator.GenerateInput) bool {
				if i, ok := input.BindData.(generator.GenerateRpcData); ok {
					if i.Name == utils.SnakeCaseToPascalCase(item.Name) {
						return true
					}
				}
				return false
			}, stateChan)
			if errGenRpc := generator.GenerateRpc(projectPath, config.ProjectName, resource.Functions, captureFunc); errGenRpc != nil {
				errChan <- errGenRpc
			}
			GenerateLogger.Info("finish - generate roles")
		}

		if len(resource.Storages) > 0 {
			GenerateLogger.Info("start - generate storages")
			captureFunc := ImportDecorateFunc(resource.Storages, func(item objects.Bucket, input generator.GenerateInput) bool {
				if i, ok := input.BindData.(generator.GenerateStoragesData); ok {
					if utils.ToSnakeCase(i.Name) == utils.ToSnakeCase(item.Name) {
						return true
					}
				}
				return false
			}, stateChan)
			if errGenStorage := generator.GenerateStorages(projectPath, resource.Storages, captureFunc); errGenStorage != nil {
				errChan <- errGenStorage
			}
			GenerateLogger.Info("finish - generate storages")
		}
	}()

	go func() {
		wg.Wait()
		close(stateChan)
		close(errChan)
	}()

	for {
		select {
		case rsErr := <-errChan:
			if rsErr != nil {
				return rsErr
			}
		case saveErr := <-doneListen:
			return saveErr
		}
	}
}

func buildGenerateModelInputs(tables []objects.Table, policies objects.Policies) []*generator.GenerateModelInput {
	mapTable := tableToMap(tables)
	mapRelations := buildGenerateMapRelations(mapTable)
	return buildGenerateModelInput(mapTable, mapRelations, policies)
}

// ---- build table relation for generated -----
type (
	MapRelations    map[string][]*state.Relation
	ManyToManyTable struct {
		Table      string
		Schema     string
		PivotTable string
		PrimaryKey string
		ForeignKey string
	}
)

func buildGenerateMapRelations(mapTable MapTable) MapRelations {
	mr := make(MapRelations)
	for _, t := range mapTable {
		r, m2m := scanGenerateTableRelation(t)
		if len(r) == 0 {
			continue
		}

		// merge with existing relation
		mergeGenerateRelations(t, r, mr)

		// merge many to many candidate with table relations
		mergeGenerateManyToManyCandidate(m2m, mr)
	}
	return mr
}

func scanGenerateTableRelation(table *objects.Table) (relations []*state.Relation, manyToManyCandidates []*ManyToManyTable) {
	// skip process if doesn`t have relation`
	if len(table.Relationships) == 0 {
		return
	}

	for _, r := range table.Relationships {
		var tableName string
		var primaryKey = r.TargetColumnName
		var foreignKey = r.SourceColumnName
		var typePrefix = "*"
		var relationType = raiden.RelationTypeHasMany

		if r.SourceTableName == table.Name {
			relationType = raiden.RelationTypeHasOne
			tableName = r.TargetTableName

			// hasOne relation is candidate to many to many relation
			// assumption table :
			//  table :
			// 		- teacher
			// 		- topic
			// 		- class
			// 	relation :
			// 		- teacher has many class
			// 		- topic has many class
			// 		- class has one teacher and has one topic
			manyToManyCandidates = append(manyToManyCandidates, &ManyToManyTable{
				Table:      r.TargetTableName,
				PivotTable: table.Name,
				PrimaryKey: r.TargetColumnName,
				ForeignKey: r.SourceColumnName,
				Schema:     r.TargetTableSchema,
			})
		} else {
			typePrefix = "[]*"
			tableName = r.SourceTableName
		}

		relation := state.Relation{
			Table:        tableName,
			Type:         typePrefix + utils.SnakeCaseToPascalCase(tableName),
			RelationType: relationType,
			PrimaryKey:   primaryKey,
			ForeignKey:   foreignKey,
		}

		relations = append(relations, &relation)
	}

	return
}

func mergeGenerateRelations(table *objects.Table, relations []*state.Relation, mapRelations MapRelations) {
	key := getMapTableKey(table.Schema, table.Name)
	tableRelations, isExist := mapRelations[key]
	if isExist {
		tableRelations = append(tableRelations, relations...)
	} else {
		tableRelations = relations
	}
	mapRelations[key] = tableRelations
}

func mergeGenerateManyToManyCandidate(candidates []*ManyToManyTable, mapRelations MapRelations) {
	for sourceTableIndex, sourceTable := range candidates {
		for targetTableIndex, targetTable := range candidates {
			if sourceTableIndex == targetTableIndex {
				continue
			}

			if sourceTable == nil || targetTable == nil {
				continue
			}

			key := getMapTableKey(sourceTable.Schema, sourceTable.Table)
			rs, exist := mapRelations[key]
			if !exist {
				rs = make([]*state.Relation, 0)
			}

			r := state.Relation{
				Table:        targetTable.Table,
				Type:         "[]*" + utils.SnakeCaseToPascalCase(targetTable.Table),
				RelationType: raiden.RelationTypeManyToMany,
				JoinRelation: &state.JoinRelation{
					Through: sourceTable.PivotTable,

					SourcePrimaryKey:      sourceTable.PrimaryKey,
					JoinsSourceForeignKey: sourceTable.ForeignKey,

					TargetPrimaryKey:     targetTable.PrimaryKey,
					JoinTargetForeignKey: targetTable.ForeignKey,
				},
			}

			rs = append(rs, &r)
			mapRelations[key] = rs
		}

	}
}

// --- attach relation to table
func buildGenerateModelInput(mapTable MapTable, mapRelations MapRelations, policies objects.Policies) []*generator.GenerateModelInput {
	generateInputs := make([]*generator.GenerateModelInput, 0)
	for k, v := range mapTable {
		input := generator.GenerateModelInput{
			Table:    *v,
			Policies: policies.FilterByTable(v.Name),
		}

		if r, exist := mapRelations[k]; exist {
			for _, v := range r {
				if v != nil {
					input.Relations = append(input.Relations, *v)
				}
			}
		}

		generateInputs = append(generateInputs, &input)
	}
	return generateInputs
}
