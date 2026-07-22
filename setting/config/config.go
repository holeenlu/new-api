package config

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
)

// ConfigManager 统一管理所有配置
type ConfigManager struct {
	configs map[string]interface{}
	mutex   sync.RWMutex
}

var GlobalConfig = NewConfigManager()

func NewConfigManager() *ConfigManager {
	return &ConfigManager{
		configs: make(map[string]interface{}),
	}
}

// Register 注册一个配置模块
func (cm *ConfigManager) Register(name string, config interface{}) {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()
	cm.configs[name] = config
}

// Get 获取指定配置模块
func (cm *ConfigManager) Get(name string) interface{} {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()
	return cm.configs[name]
}

// LoadFromDB 从数据库加载配置
func (cm *ConfigManager) LoadFromDB(options map[string]string) error {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	var updateErrors []error
	for name, config := range cm.configs {
		prefix := name + "."
		configMap := make(map[string]string)

		// 收集属于此配置的所有选项
		for key, value := range options {
			if strings.HasPrefix(key, prefix) {
				configKey := strings.TrimPrefix(key, prefix)
				configMap[configKey] = value
			}
		}

		// 如果找到配置项，则更新配置
		if len(configMap) > 0 {
			if err := updateConfigFromMap(config, configMap); err != nil {
				common.SysError("failed to update config " + name + ": " + err.Error())
				updateErrors = append(updateErrors, fmt.Errorf("update config %s: %w", name, err))
				continue
			}
		}
	}

	return errors.Join(updateErrors...)
}

// SaveToDB 将配置保存到数据库
func (cm *ConfigManager) SaveToDB(updateFunc func(key, value string) error) error {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	for name, config := range cm.configs {
		configMap, err := configToMap(config)
		if err != nil {
			return err
		}

		for key, value := range configMap {
			dbKey := name + "." + key
			if err := updateFunc(dbKey, value); err != nil {
				return err
			}
		}
	}

	return nil
}

// 辅助函数：将配置对象转换为map
func configToMap(config interface{}) (map[string]string, error) {
	if managed, ok := config.(managedConfig); ok {
		snapshot, err := managed.snapshotForConfig()
		if err != nil {
			return nil, err
		}
		return configToMap(snapshot)
	}

	result := make(map[string]string)

	val := reflect.ValueOf(config)
	if val.Kind() == reflect.Ptr {
		val = val.Elem()
	}

	if val.Kind() != reflect.Struct {
		return nil, nil
	}

	typ := val.Type()
	for i := 0; i < val.NumField(); i++ {
		field := val.Field(i)
		fieldType := typ.Field(i)

		// 跳过未导出字段
		if !fieldType.IsExported() {
			continue
		}

		// 获取json标签作为键名
		key := fieldType.Tag.Get("json")
		if key == "" || key == "-" {
			key = fieldType.Name
		}

		// 处理不同类型的字段
		var strValue string
		switch field.Kind() {
		case reflect.String:
			strValue = field.String()
		case reflect.Bool:
			strValue = strconv.FormatBool(field.Bool())
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			strValue = strconv.FormatInt(field.Int(), 10)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			strValue = strconv.FormatUint(field.Uint(), 10)
		case reflect.Float32, reflect.Float64:
			strValue = strconv.FormatFloat(field.Float(), 'f', -1, 64)
		case reflect.Ptr:
			// 处理指针类型：如果非 nil，序列化指向的值
			if !field.IsNil() {
				bytes, err := common.Marshal(field.Interface())
				if err != nil {
					return nil, err
				}
				strValue = string(bytes)
			} else {
				// nil 指针序列化为 "null"
				strValue = "null"
			}
		case reflect.Map, reflect.Slice, reflect.Struct:
			// 复杂类型使用JSON序列化
			bytes, err := common.Marshal(field.Interface())
			if err != nil {
				return nil, err
			}
			strValue = string(bytes)
		default:
			// 跳过不支持的类型
			continue
		}

		result[key] = strValue
	}

	return result, nil
}

// 辅助函数：从map更新配置对象
func updateConfigFromMap(config interface{}, configMap map[string]string) error {
	if managed, ok := config.(managedConfig); ok {
		return managed.updateFromConfigMap(configMap)
	}
	return updateStructFromMap(config, configMap, false)
}

// updateStructFromMap updates a regular struct in place. Strict mode is used
// by managed configurations: every supplied key must be recognized and every
// value must parse successfully before the caller publishes its private copy.
// Legacy configurations retain the historical best-effort parsing behavior.
func updateStructFromMap(config interface{}, configMap map[string]string, strict bool) error {
	val := reflect.ValueOf(config)
	if val.Kind() != reflect.Ptr {
		if strict {
			return fmt.Errorf("configuration target must be a pointer, got %T", config)
		}
		return nil
	}
	val = val.Elem()

	if val.Kind() != reflect.Struct {
		if strict {
			return fmt.Errorf("configuration target must point to a struct, got %T", config)
		}
		return nil
	}

	typ := val.Type()
	updatedKeys := make(map[string]struct{}, len(configMap))
	for i := 0; i < val.NumField(); i++ {
		field := val.Field(i)
		fieldType := typ.Field(i)

		// 跳过未导出字段
		if !fieldType.IsExported() {
			continue
		}

		// 获取json标签作为键名
		key := fieldType.Tag.Get("json")
		if key == "" || key == "-" {
			key = fieldType.Name
		}

		// 检查map中是否有对应的值
		strValue, ok := configMap[key]
		if !ok {
			continue
		}
		updatedKeys[key] = struct{}{}

		// 根据字段类型设置值
		if !field.CanSet() {
			if strict {
				return fmt.Errorf("config field %q cannot be set", key)
			}
			continue
		}

		switch field.Kind() {
		case reflect.String:
			field.SetString(strValue)
		case reflect.Bool:
			boolValue, err := strconv.ParseBool(strValue)
			if err != nil {
				if strict {
					return fmt.Errorf("config field %q must be a boolean: %w", key, err)
				}
				continue
			}
			field.SetBool(boolValue)
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			bits := field.Type().Bits()
			intValue, err := strconv.ParseInt(strValue, 10, bits)
			if err != nil {
				// 兼容 float 格式的字符串（如 "2.000000"）
				floatValue, fErr := strconv.ParseFloat(strValue, 64)
				limit := math.Ldexp(1, bits-1)
				if fErr != nil || math.IsNaN(floatValue) || math.IsInf(floatValue, 0) ||
					math.Trunc(floatValue) != floatValue || floatValue < -limit || floatValue >= limit {
					if strict {
						return fmt.Errorf("config field %q must be a %d-bit integer", key, bits)
					}
					continue
				}
				intValue = int64(floatValue)
			}
			field.SetInt(intValue)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			bits := field.Type().Bits()
			uintValue, err := strconv.ParseUint(strValue, 10, bits)
			if err != nil {
				// 兼容 float 格式的字符串
				floatValue, fErr := strconv.ParseFloat(strValue, 64)
				limit := math.Ldexp(1, bits)
				if fErr != nil || math.IsNaN(floatValue) || math.IsInf(floatValue, 0) ||
					math.Trunc(floatValue) != floatValue || floatValue < 0 || floatValue >= limit {
					if strict {
						return fmt.Errorf("config field %q must be an unsigned %d-bit integer", key, bits)
					}
					continue
				}
				uintValue = uint64(floatValue)
			}
			field.SetUint(uintValue)
		case reflect.Float32, reflect.Float64:
			floatValue, err := strconv.ParseFloat(strValue, field.Type().Bits())
			if err != nil || (strict && (math.IsNaN(floatValue) || math.IsInf(floatValue, 0))) {
				if strict {
					return fmt.Errorf("config field %q must be a finite number", key)
				}
				continue
			}
			field.SetFloat(floatValue)
		case reflect.Ptr:
			// 处理指针类型
			if strValue == "null" {
				field.Set(reflect.Zero(field.Type()))
			} else if !strict {
				// Legacy pointer-backed settings may intentionally share the pointed-to
				// object with a package-level runtime index. Update that object in place
				// so configuration loading does not sever its ownership identity.
				if field.IsNil() {
					field.Set(reflect.New(field.Type().Elem()))
				}
				if err := common.Unmarshal([]byte(strValue), field.Interface()); err != nil {
					continue
				}
			} else {
				fresh := reflect.New(field.Type().Elem())
				if !field.IsNil() {
					fresh.Elem().Set(field.Elem())
				}
				if err := common.Unmarshal([]byte(strValue), fresh.Interface()); err != nil {
					if strict {
						return fmt.Errorf("config field %q contains invalid JSON: %w", key, err)
					}
					continue
				}
				field.Set(fresh)
			}
		case reflect.Map:
			// json.Unmarshal merges into existing maps (keeps old keys that are
			// absent from the new JSON). Allocate a fresh map so removed keys
			// are properly cleared.
			fresh := reflect.New(field.Type())
			if err := common.Unmarshal([]byte(strValue), fresh.Interface()); err != nil {
				if strict {
					return fmt.Errorf("config field %q contains invalid JSON: %w", key, err)
				}
				continue
			}
			field.Set(fresh.Elem())
		case reflect.Slice:
			if !strict {
				if err := common.Unmarshal([]byte(strValue), field.Addr().Interface()); err != nil {
					continue
				}
				break
			}
			fresh := reflect.New(field.Type())
			if err := common.Unmarshal([]byte(strValue), fresh.Interface()); err != nil {
				if strict {
					return fmt.Errorf("config field %q contains invalid JSON: %w", key, err)
				}
				continue
			}
			field.Set(fresh.Elem())
		case reflect.Struct:
			if !strict {
				if err := common.Unmarshal([]byte(strValue), field.Addr().Interface()); err != nil {
					continue
				}
				break
			}
			fresh := reflect.New(field.Type())
			fresh.Elem().Set(field)
			if err := common.Unmarshal([]byte(strValue), fresh.Interface()); err != nil {
				if strict {
					return fmt.Errorf("config field %q contains invalid JSON: %w", key, err)
				}
				continue
			}
			field.Set(fresh.Elem())
		default:
			if strict {
				return fmt.Errorf("config field %q has unsupported type %s", key, field.Type())
			}
		}
	}

	if strict {
		for key := range configMap {
			if _, ok := updatedKeys[key]; !ok {
				return fmt.Errorf("unknown config field %q", key)
			}
		}
	}

	return nil
}

// ConfigToMap 将配置对象转换为map（导出函数）
func ConfigToMap(config interface{}) (map[string]string, error) {
	return configToMap(config)
}

// UpdateConfigFromMap 从map更新配置对象（导出函数）
func UpdateConfigFromMap(config interface{}, configMap map[string]string) error {
	return updateConfigFromMap(config, configMap)
}

// ValidateConfigUpdate verifies a configuration update without publishing it.
// Managed configurations validate a copy of their current snapshot; legacy
// configurations parse the supplied fields into a fresh value of the same type.
func ValidateConfigUpdate(config interface{}, configMap map[string]string) error {
	if managed, ok := config.(managedConfig); ok {
		return managed.validateConfigMap(configMap)
	}

	value := reflect.ValueOf(config)
	if value.Kind() != reflect.Ptr || value.IsNil() || value.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("configuration target must point to a struct, got %T", config)
	}

	// Legacy configurations are still updated in place for compatibility, but
	// validation must never mutate them. Parse the requested fields into a fresh
	// value of the same type so UpdateOptionsBulk can reject malformed data before
	// committing its database transaction.
	candidate := reflect.New(value.Elem().Type())
	return updateStructFromMap(candidate.Interface(), configMap, true)
}

// ExportAllConfigs 导出所有已注册的配置为扁平结构
func (cm *ConfigManager) ExportAllConfigs() map[string]string {
	result, err := cm.ExportAllConfigsChecked()
	if err != nil {
		common.SysError("failed to export registered configs: " + err.Error())
	}
	return result
}

// ExportAllConfigsChecked exports every registered configuration and returns
// serialization errors instead of silently dropping a module.
func (cm *ConfigManager) ExportAllConfigsChecked() (map[string]string, error) {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	result := make(map[string]string)

	for name, cfg := range cm.configs {
		configMap, err := ConfigToMap(cfg)
		if err != nil {
			return result, fmt.Errorf("export config %s: %w", name, err)
		}

		// 使用 "模块名.配置项" 的格式添加到结果中
		for key, value := range configMap {
			result[name+"."+key] = value
		}
	}

	return result, nil
}
