package validators

import (
	"errors"

	l "github.com/redhatinsights/insights-ingress-go/logger"
)

type MultiValidator struct {
	Validators []Validator
}

func (m *MultiValidator) getFirstValidator(service *ServiceDescriptor) Validator {
	for _, v := range m.Validators {
		if err := v.ValidateService(service); err == nil {
			return v
		}
	}
	return nil
}

// ValidateService checks each validator for validation
func (m *MultiValidator) ValidateService(service *ServiceDescriptor) error {
	if v := m.getFirstValidator(service); v == nil {
		return errors.New("Service is not Valid")
	}
	return nil
}

func (m *MultiValidator) Validate(request *Request) {
	if v := m.getFirstValidator(&ServiceDescriptor{
		Service:  request.Service,
		Category: request.Category,
	}); v == nil {
		l.Log.Error("Validate called with a service with no validator")
	}
}
