// Copyright 2015 Keybase, Inc. All rights reserved. Use of
// this source code is governed by the included BSD license.

package engine

import (
	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/protocol/keybase1"
)

// LoginProvisionedDevice is an engine that tries to login using the
// current device, if there is an existing provisioned device.
type LoginProvisionedDevice struct {
	libkb.Contextified
	username        libkb.NormalizedUsername
	SecretStoreOnly bool // this should only be set by the service on its startup login attempt
}

// newLoginCurrentDevice creates a loginProvisionedDevice engine.
func NewLoginProvisionedDevice(g *libkb.GlobalContext, username string) *LoginProvisionedDevice {
	return &LoginProvisionedDevice{
		username:     libkb.NewNormalizedUsername(username),
		Contextified: libkb.NewContextified(g),
	}
}

// Name is the unique engine name.
func (e *LoginProvisionedDevice) Name() string {
	return "LoginProvisionedDevice"
}

// GetPrereqs returns the engine prereqs.
func (e *LoginProvisionedDevice) Prereqs() Prereqs {
	return Prereqs{}
}

// RequiredUIs returns the required UIs.
func (e *LoginProvisionedDevice) RequiredUIs() []libkb.UIKind {
	if e.SecretStoreOnly {
		return []libkb.UIKind{}
	}

	return []libkb.UIKind{
		libkb.LoginUIKind,
		libkb.SecretUIKind,
	}
}

// SubConsumers returns the other UI consumers for this engine.
func (e *LoginProvisionedDevice) SubConsumers() []libkb.UIConsumer {
	return nil
}

func (e *LoginProvisionedDevice) Run(m libkb.MetaContext) error {
	if err := e.run(m); err != nil {
		return err
	}

	m.CDebugf("LoginProvisionedDevice success, sending login notification")
	m.G().NotifyRouter.HandleLogin(string(m.G().Env.GetUsername()))
	m.CDebugf("LoginProvisionedDevice success, calling login hooks")
	m.G().CallLoginHooks()

	return nil
}

func (e *LoginProvisionedDevice) loadMe(m libkb.MetaContext) (err error) {
	defer m.CTrace("LoginProvisionedDevice#loadMe", func() error { return err })()

	var config *libkb.UserConfig
	var nu libkb.NormalizedUsername
	loadUserArg := libkb.NewLoadUserArgWithMetaContext(m).WithPublicKeyOptional().WithForcePoll(true)
	if len(e.username) == 0 {
		m.CDebugf("| using current username")
		config, err = m.G().Env.GetConfig().GetUserConfig()
		loadUserArg = loadUserArg.WithSelf(true)
	} else {
		m.CDebugf("| using new username %s", e.username)
		nu = e.username
		config, err = m.G().Env.GetConfig().GetUserConfigForUsername(nu)
		loadUserArg = loadUserArg.WithName(e.username.String())
	}
	if err != nil {
		m.CDebugf("error getting user config: %s (%T)", err, err)
		return errNoConfig
	}
	if config == nil {
		m.CDebugf("user config is nil")
		return errNoConfig
	}
	deviceID := config.GetDeviceID()
	if deviceID.IsNil() {
		m.CDebugf("no device in user config")
		return errNoDevice
	}

	// Make sure the device ID is still valid.
	upak, _, err := m.G().GetUPAKLoader().LoadV2(loadUserArg)
	if err != nil {
		m.CDebugf("error loading user profile: %#v", err)
		return err
	}
	if upak.Current.Status == keybase1.StatusCode_SCDeleted {
		m.CDebugf("User %s was deleted", upak.Current.Uid)
		return libkb.UserDeletedError{}
	}

	nu = libkb.NewNormalizedUsername(upak.Current.Username)
	device := upak.Current.FindSigningDeviceKey(deviceID)

	nukeDevice := false
	if device == nil {
		m.CDebugf("Current device %s not found", deviceID)
		nukeDevice = true
	} else if device.Base.Revocation != nil {
		m.CDebugf("Current device %s has been revoked", deviceID)
		nukeDevice = true
	}

	if nukeDevice {
		// If our config file is showing that we have a bogus
		// deviceID (maybe from our account before an account reset),
		// then we'll delete it from the config file here, so later parts
		// of provisioning aren't confused by this device ID.
		tmp := m.SwitchUserNukeConfig(nu)
		if tmp != nil {
			m.CWarningf("Error clearing user config: %s", tmp)
		}
		return errNoDevice
	}

	e.username = nu
	return nil
}

func (e *LoginProvisionedDevice) run(m libkb.MetaContext) (err error) {
	defer m.CTrace("LoginProvisionedDevice#run", func() error { return err })()

	// already logged in?
	in, uid := isLoggedIn(m)
	if in && (len(e.username) == 0 || m.G().Env.GetUsernameForUID(uid).Eq(e.username)) {
		m.CDebugf("user %s already logged in; short-circuting", uid)
		return nil
	}

	err = e.loadMe(m)
	if err != nil {
		return nil
	}

	// at this point, there is a user config either for the current user or for e.username
	// and it has a device id, so this should be a provisioned device.  Thus, they should
	// just login normally.

	var afterLogin = func(lctx libkb.LoginContext) error {
		if err := lctx.LocalSession().SetDeviceProvisioned(m.G().Env.GetDeviceID()); err != nil {
			// not a fatal error, session will stay in memory
			m.CWarningf("error saving session file: %s", err)
		}
		return nil
	}

	if e.SecretStoreOnly {
		if err := m.G().LoginState().LoginWithStoredSecret(m, e.username.String(), afterLogin); err != nil {
			return err
		}

	} else {
		if err := m.G().LoginState().LoginWithPrompt(m, e.username.String(), m.UIs().LoginUI, m.UIs().SecretUI, afterLogin); err != nil {
			return err
		}
	}

	// login was successful, unlock the device keys
	// err = e.unlockDeviceKeys(m, me)
	if err != nil {
		return err
	}
	return nil
}

func (e *LoginProvisionedDevice) unlockDeviceKeys(m libkb.MetaContext, me *libkb.User) error {

	// CORE-5876 idea that lksec will be unusable if reachability state is NO
	// and the user changed passphrase with a different device since it won't
	// be able to sync the new server half.
	if m.G().ConnectivityMonitor.IsConnected(m.Ctx()) != libkb.ConnectivityMonitorYes {
		m.CDebugf("LoginProvisionedDevice: in unlockDeviceKeys, ConnectivityMonitor says not reachable, check to make sure")
		if err := m.G().ConnectivityMonitor.CheckReachability(m.Ctx()); err != nil {
			m.CDebugf("error checking reachability: %s", err)
		} else {
			connected := m.G().ConnectivityMonitor.IsConnected(m.Ctx())
			m.CDebugf("after CheckReachability(), IsConnected() => %v (connected? %v)", connected, connected == libkb.ConnectivityMonitorYes)
		}
	}

	ska := libkb.SecretKeyArg{
		Me:      me,
		KeyType: libkb.DeviceSigningKeyType,
	}
	_, err := m.G().Keyrings.GetSecretKeyWithPrompt(m, m.SecretKeyPromptArg(ska, "unlock device keys"))
	if err != nil {
		return err
	}
	ska.KeyType = libkb.DeviceEncryptionKeyType
	_, err = m.G().Keyrings.GetSecretKeyWithPrompt(m, m.SecretKeyPromptArg(ska, "unlock device keys"))
	if err != nil {
		return err
	}

	return nil
}
