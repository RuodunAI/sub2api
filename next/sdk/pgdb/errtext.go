package pgdb

import (
	"slices"

	"gorm.io/gorm"
)

// driverWording is the dialector of the handles Open returns: their
// translated errors keep the driver's text. GORM translates unique, foreign
// key and check violations and unknown columns to the gorm errors errors.Is
// tells apart (gorm.ErrDuplicatedKey, ...) and puts their text in front of
// the driver's: "duplicated key not allowed: ERROR: duplicate key value ...".
// Plugins show error texts to administrators (an endpoint's message, a job's
// result, an event delivery's last error) and showed the driver's before
// they were on GORM. Such an error is still gorm.ErrDuplicatedKey and
// friends for errors.Is, and still the driver's error for errors.As, but its
// text is the driver's alone.
// translations are the errors GORM's postgres dialector translates a driver
// error to, wrapping both ("%w: %w").
var translations = []error{gorm.ErrDuplicatedKey, gorm.ErrForeignKeyViolated, gorm.ErrInvalidField, gorm.ErrCheckConstraintViolated}

type driverWording struct{ gorm.Dialector }

// Translate implements gorm.ErrorTranslator.
func (d driverWording) Translate(err error) error {
	t, ok := d.Dialector.(gorm.ErrorTranslator)
	if !ok {
		return err
	}
	translated := t.Translate(err)
	both, ok := translated.(interface{ Unwrap() []error })
	if !ok {
		return translated
	}
	if errs := both.Unwrap(); len(errs) == 2 && slices.Contains(translations, errs[0]) {
		return &driverError{kind: errs[0], err: errs[1]}
	}
	return translated
}

// SavePoint implements gorm.SavePointerDialectorInterface (a transaction in
// a transaction).
func (d driverWording) SavePoint(tx *gorm.DB, name string) error {
	if s, ok := d.Dialector.(gorm.SavePointerDialectorInterface); ok {
		return s.SavePoint(tx, name)
	}
	return gorm.ErrUnsupportedDriver
}

// RollbackTo implements gorm.SavePointerDialectorInterface.
func (d driverWording) RollbackTo(tx *gorm.DB, name string) error {
	if s, ok := d.Dialector.(gorm.SavePointerDialectorInterface); ok {
		return s.RollbackTo(tx, name)
	}
	return gorm.ErrUnsupportedDriver
}

// driverError is a translated error with the driver's text: errors.Is
// matches kind (gorm.ErrDuplicatedKey, ...), errors.As the driver's error.
type driverError struct{ kind, err error }

func (e *driverError) Error() string { return e.err.Error() }

func (e *driverError) Unwrap() []error { return []error{e.kind, e.err} }
