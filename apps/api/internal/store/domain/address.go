package domain

// Address is the store's postal address value group. It is materialized with
// empty strings for unpopulated fields so the presentation layer has a stable
// shape (never a null object).
type Address struct {
	street        string
	number        string
	complement    string
	district      string
	city          string
	state         string
	zip           string
	country       string
	stateRegister string
}

// ReconstructAddress rebuilds an Address from persistence data (trusted, no
// validation).
func ReconstructAddress(
	street, number, complement, district, city, state, zip, country, stateRegister string,
) Address {
	return Address{
		street:        street,
		number:        number,
		complement:    complement,
		district:      district,
		city:          city,
		state:         state,
		zip:           zip,
		country:       country,
		stateRegister: stateRegister,
	}
}

func (a Address) Street() string        { return a.street }
func (a Address) Number() string        { return a.number }
func (a Address) Complement() string    { return a.complement }
func (a Address) District() string      { return a.district }
func (a Address) City() string          { return a.city }
func (a Address) State() string         { return a.state }
func (a Address) Zip() string           { return a.zip }
func (a Address) Country() string       { return a.country }
func (a Address) StateRegister() string { return a.stateRegister }
