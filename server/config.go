package server

import (
	"sync"

	"github.com/teacat/chaturbate-dvr/entity"
)

var Config *entity.Config
var ConfigMu sync.RWMutex
