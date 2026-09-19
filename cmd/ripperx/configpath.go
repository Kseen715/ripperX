package main

// Where the settings file lives when none is named on the command line.
// It is a system path because ripperX drives hardware that belongs to the
// machine rather than to a user, and is normally run as a service.
const defaultConfigPath = "/etc/ripperx.conf"
