package database

import (
	"encoding/json"
	"strings"

	"github.com/de0gee/datastore/src/sensor"
	_ "github.com/mattn/go-sqlite3"
	"github.com/pkg/errors"
	"github.com/schollz/mapslimmer"
)

func (d *Database) MakeTables() (err error) {
	sqlStmt := `create table keystore (key text not null primary key, value text);`
	_, err = d.db.Exec(sqlStmt)
	if err != nil {
		err = errors.Wrap(err, "MakeTables")
		d.logger.Error(err)
		return
	}
	sqlStmt = `create index keystore_idx on keystore(key);`
	_, err = d.db.Exec(sqlStmt)
	if err != nil {
		err = errors.Wrap(err, "MakeTables")
		d.logger.Error(err)
		return
	}
	sqlStmt = `create table sensors (timestamp integer not null primary key, family text, device text, unique(timestamp));`
	_, err = d.db.Exec(sqlStmt)
	if err != nil {
		err = errors.Wrap(err, "MakeTables")
		d.logger.Error(err)
		return
	}
	return
}

// Columns will list the columns
func (d *Database) Columns() (columns []string, err error) {
	rows, err := d.db.Query("select * from sensors limit 1")
	if err != nil {
		err = errors.Wrap(err, "Columns")
		return
	}
	columns, err = rows.Columns()
	rows.Close()
	if err != nil {
		err = errors.Wrap(err, "Columns")
		return
	}
	d.logger.Info("listed columns")
	return
}

// Get will retrieve the value associated with a key.
func (d *Database) Get(key string, v interface{}) (err error) {
	stmt, err := d.db.Prepare("select value from keystore where key = ?")
	if err != nil {
		return errors.Wrap(err, "problem preparing SQL")
	}
	defer stmt.Close()
	var result string
	err = stmt.QueryRow(key).Scan(&result)
	if err != nil {
		return errors.Wrap(err, "problem getting key")
	}

	err = json.Unmarshal([]byte(result), &v)
	if err != nil {
		return
	}
	d.logger.Infof("got %s from '%s'", string(result), key)
	return
}

// Set will set a value in the database, when using it like a keystore.
func (d *Database) Set(key string, value interface{}) (err error) {
	var b []byte
	b, err = json.Marshal(value)
	if err != nil {
		return err
	}
	tx, err := d.db.Begin()
	if err != nil {
		return errors.Wrap(err, "Set")
	}
	stmt, err := tx.Prepare("insert or replace into keystore(key,value) values (?, ?)")
	if err != nil {
		return errors.Wrap(err, "Set")
	}
	defer stmt.Close()

	_, err = stmt.Exec(key, string(b))
	if err != nil {
		return errors.Wrap(err, "Set")
	}

	err = tx.Commit()
	if err != nil {
		return errors.Wrap(err, "Set")
	}

	d.logger.Infof("set '%s' to '%s'", key, string(b))
	return
}

// GetSensorFromTime will return a sensor data for a given timestamp
func (d *Database) GetSensorFromTime(timestamp int) (s sensor.Data, err error) {
	// first get the columns
	columnList, err := d.Columns()
	if err != nil {
		return
	}

	// get the slimmer
	var slimmer string
	err = d.Get("slimmer", &slimmer)
	if err != nil {
		return
	}
	ms, err := mapslimmer.Init(slimmer)
	if err != nil {
		return
	}

	// prepare statement
	stmt, err := d.db.Prepare("select * from sensors where timestamp = ?")
	if err != nil {
		err = errors.Wrap(err, "GetSensorFromTime")
		return
	}
	defer stmt.Close()
	var arr []interface{}
	for i := 0; i < len(columnList); i++ {
		arr = append(arr, new(interface{}))
	}
	err = stmt.QueryRow(timestamp).Scan(arr...)
	if err != nil {
		err = errors.Wrap(err, "GetSensorFromTime")
		return
	}

	s = sensor.Data{
		// the underlying value of the interface pointer and cast it to a pointer interface to cast to a byte to cast to a string
		Timestamp: int((*arr[0].(*interface{})).(int64)),
		Family:    string((*arr[1].(*interface{})).([]uint8)),
		Device:    string((*arr[2].(*interface{})).([]uint8)),
		Sensors:   make(map[string]map[string]interface{}),
	}
	// add in the sensor data
	for i, colName := range columnList {
		if i < 3 {
			continue
		}
		unslimmed := string((*arr[i].(*interface{})).([]uint8))
		s.Sensors[colName], err = ms.Loads(unslimmed)
		if err != nil {
			return
		}
	}
	return
}

// AddSensor will insert a sensor data into the database
// TODO: AddSensor should be special case of AddSensors
func (d *Database) AddSensor(s sensor.Data) (err error) {
	// determine the current table colums
	oldColumns := make(map[string]struct{})
	columnList, err := d.Columns()
	if err != nil {
		return
	}
	for _, column := range columnList {
		oldColumns[column] = struct{}{}
	}

	// get slimmer
	var slimmer string
	err = d.Get("slimmer", &slimmer)
	if err != nil {
		return
	}
	ms, err := mapslimmer.Init(slimmer)
	if err != nil {
		return
	}

	// setup the database
	tx, err := d.db.Begin()
	if err != nil {
		return errors.Wrap(err, "AddSensor")
	}

	// first add new columns in the sensor data
	args := make([]interface{}, 3)
	args[0] = s.Timestamp
	args[1] = s.Family
	args[2] = s.Device
	argsQ := []string{"?", "?", "?"}
	for sensor := range s.Sensors {
		if _, ok := oldColumns[sensor]; !ok {
			stmt, err := tx.Prepare("alter table sensors add column " + sensor + " text")
			if err != nil {
				return errors.Wrap(err, "AddSensor, adding column")
			}
			_, err = stmt.Exec()
			if err != nil {
				return errors.Wrap(err, "AddSensor, adding column")
			}
			d.logger.Infof("adding column %s", sensor)
			columnList = append(columnList, sensor)
			stmt.Close()
		}
	}

	// organize arguments in the correct order
	for _, sensor := range columnList {
		if _, ok := s.Sensors[sensor]; !ok {
			continue
		}
		argsQ = append(argsQ, "?")
		args = append(args, ms.Dumps(s.Sensors[sensor]))
	}

	// insert the new data
	sqlStatement := "insert or replace into sensors(" + strings.Join(columnList, ",") + ") values (" + strings.Join(argsQ, ",") + ")"
	stmt, err := tx.Prepare(sqlStatement)
	d.logger.Info("columns", columnList)
	d.logger.Info("args", args)
	if err != nil {
		return errors.Wrap(err, "AddSensor, prepare")
	}
	defer stmt.Close()

	_, err = stmt.Exec(args...)
	if err != nil {
		return errors.Wrap(err, "AddSensor, execute")
	}

	err = tx.Commit()
	if err != nil {
		return errors.Wrap(err, "AddSensor")
	}

	// update the map key slimmer
	d.Set("slimmer", ms.Slimmer())

	d.logger.Info("inserted sensor data")
	return

}
