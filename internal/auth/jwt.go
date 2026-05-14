package auth

import (
	"errors"
	"fmt"

	"github.com/golang-jwt/jwt/v5"
)

type jwtClaims struct {
	jwt.RegisteredClaims
	Channels []string `json:"channels"`
}

func (s *authService) ParseAndValidateToken(tokenString string) (*Claims, error) {
	claims := &jwtClaims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, s.keyFunc,
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithLeeway(s.clockSkew),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, fmt.Errorf("token expired: %w", ErrTokenExpired)
		}
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	if !token.Valid {
		return nil, ErrInvalidToken
	}

	subject, err := claims.GetSubject()
	if err != nil {
		return nil, fmt.Errorf("get subject: %w", ErrInvalidToken)
	}

	return &Claims{
		Subject:  subject,
		Channels: claims.Channels,
	}, nil
}

func (s *authService) keyFunc(token *jwt.Token) (interface{}, error) {
	if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
		return nil, ErrInvalidAlgorithm
	}
	if token.Method.Alg() != "HS256" {
		return nil, ErrInvalidAlgorithm
	}
	return s.signingKey, nil
}
