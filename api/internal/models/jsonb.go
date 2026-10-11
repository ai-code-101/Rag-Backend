package models

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
)

// JSONB is raw JSON stored in a jsonb column.
type JSONB json.RawMessage

func (j JSONB) Value() (driver.Value, error) {
	if len(j) == 0 {
		return "{}", nil
	}
	return string(j), nil
}

func (j *JSONB) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*j = JSONB("{}")
	case []byte:
		*j = append((*j)[:0], v...)
	case string:
		*j = JSONB(v)
	default:
		return errors.New("models.JSONB: unsupported scan type")
	}
	return nil
}

func (j JSONB) MarshalJSON() ([]byte, error) {
	if len(j) == 0 {
		return []byte("{}"), nil
	}
	return j, nil
}

func (j *JSONB) UnmarshalJSON(b []byte) error {
	*j = append((*j)[:0], b...)
	return nil
}

func (JSONB) GormDataType() string { return "jsonb" }
