// Copyright (c) 2015 Pagoda Box Inc
//
// This Source Code Form is subject to the terms of the Mozilla Public License, v.
// 2.0. If a copy of the MPL was not distributed with this file, You can obtain one
// at http://mozilla.org/MPL/2.0/.
//

package main

import (
	"fmt"
	"time"
)

var lastKnownCluster []Status

// Starts a goroutine that makes all the decisions
// about who is to become master/slave/single and at what times
func DecisionStart() error {
	log.Info("[decision.DecisionStart]")

	// wait for the cluster to come online
	waitForClusterFull()
	// start the database and perform actions on that database
	go func() {
		self := Whoami()
		log.Debug("[decision] myself %+v", self)
		if self.CRole == "monitor" {
			log.Debug("[decision] im a monitor.. i dont make decisions")
			status.SetDBRole("monitor")
			status.SetState("running")
			return
		}
		// start the database up
		startupDB()
		lastKnownCluster = Cluster()

		// start a timer that will trigger a cluster check
		timer := make(chan bool)
		go func() {
			for {
				time.Sleep(time.Second * 10)
				timer <- true
			}
		}()

		// check server on timer and listen for advice
		// if you notice a problem perform an action
		for {
			select {

			case adv := <-advice:
				// i need a new self to see if im currently the master
				self := Whoami()
				if adv == "demote" && self.DBRole == "master" {
					updateStatusRole("dead(master)")
					actions <- "kill"
				} else {
					log.Info("[decision] got some advice:" + adv)
					// what do i do with other advice?
					log.Debug("Advice triggered check")
					if clusterChanges() {
						performAction()
					}
				}
			case <-timer:
				log.Debug("timer triggered check")
				if clusterChanges() {
					performAction()
				}
			}
		}
	}()
	return nil
}

// this will ping the cluster until it has the
// appropriate number of members
func waitForClusterFull() {
	for {
		c := Cluster()
		if len(c) == 3 {
			log.Info("[decision] members are all online!")
			return
		}

		log.Info("[decision] waiting for members (cluster(%d))\n", len(c))
		time.Sleep(time.Second)
	}
}

// figure out what to start as.
func startupDB() {
	log.Debug("[decision] Starting Db")
	self := Whoami()
	switch self.CRole {
	case "primary":
		r := startType("master")
		updateStatusRole(r)
		log.Info("[decision] I am starting as " + r)
		actions <- r
	case "secondary":
		r := startType("slave")
		updateStatusRole(r)
		log.Info("[decision] I am starting as " + r)
		actions <- r
	default:
		log.Warn("[decision] Monitors dont do anything. (and this shouldnt have been executed)")
	}
}

// takes the default starting string
// and decides how it should start
func startType(def string) string {
	self := Whoami()

	log.Debug("[decision] startType: self: %+v", self)
	switch self.DBRole {
	case "initialized":
		return def
	case "single", "dead(single)":
		return "master"
	case "master", "dead(master)":
		// check the other node and see if it is single
		// if not i stay master
		// if so i go slave
		other, _ := Whoisnot(self.CRole)
		log.Debug("[decision] startType: other: %+v", other)
		// if the other guy has transitioned to single
		if other.DBRole == "single" || other.DBRole == "dead(single)" {
			return "slave"
		}
		// if the other guy detected i came back online and is already
		// switching to master
		if other.DBRole == "master" && other.UpdatedAt.After(self.UpdatedAt) {
			return "slave"
		}
		return "master"
	case "slave", "dead(slave)":
		return "slave"
	}
	log.Error("[decision] Error: Status: %+v\n", self)
	panic("i should have caught all scenarios")
	return def
}

// detect a change in the cluster
// new members, lost members, or changes to member states
func clusterChanges() bool {
	c := Cluster()
	str := "[decision] Cluster Members Changed: "
	for _, member := range c {
		str += fmt.Sprintf("(%s:%s)", member.CRole, member.Ip)
	}
	if len(lastKnownCluster) != len(c) {
		log.Debug("[decision] The cluster size changed from %d to %d", len(lastKnownCluster), len(c))
		lastKnownCluster = c
		log.Info(str)
		return true
	}
	for _, member := range lastKnownCluster {
		remote, err := Whois(member.CRole)
		if err != nil {
			log.Debug("[decision] The remote member died while i was trying to pull its updates")
			lastKnownCluster = c
			log.Info(str)
			return true
		}
		if member.DBRole != remote.DBRole {
			log.Debug("[decision] The cluster members(%s) role changed from %s to %s", member.CRole, member.DBRole, remote.DBRole)
			lastKnownCluster = c
			log.Info(str)
			return true
		}
	}
	return false
}

// decides what state to take when clusterChanges is true
func performAction() {
	self := Whoami()
	other, _ := Whoisnot(self.CRole)

	log.Debug("[decision] performAction: \nself: %+v, \nother: %+v", self, other)
	switch self.DBRole {
	case "single":
		performActionFromSingle(self, other)
	case "master":
		performActionFromMaster(self, other)
	case "slave":
		performActionFromSlave(self, other)
	case "dead(master)", "dead(slave)", "dead(single)":
		performActionFromDead(self, other)
	}
}

// Breakdown of the performAction for when we are single
func performActionFromSingle(self, other *Status) {
	mon, _ := Whois("monitor")
	switch {
	case other == nil && mon == nil:
		// kill
		updateStatusRole("dead(single)")
		log.Info("[decision] performActionFromSingle: im alone: going dead(single)")
		actions <- "kill"
	case other == nil:
		// do nothing
		log.Info("[decision] performActionFromSingle: other is still gone: doing nothing")
	case mon == nil:
		// do nothing
		log.Info("[decision] performActionFromSingle: other back monitor gone: doing nothing")
	case other != nil && mon != nil:
		// go master
		updateStatusRole("master")
		log.Info("[decision] performActionFromSingle: other came back online: going master")
		actions <- "master"
	}
}

// Breakdown of the performAction for when we are master
func performActionFromMaster(self, other *Status) {
	if other != nil && other.DBRole == "slave" {
		// i lost the monitor
		// shouldnt hurt anything
		log.Info("[decision] performActionFromMaster: other is slave: im doing nothing")
		return
	}

	if other != nil && other.DBRole == "dead(slave)" {
		// my slave has died and i need to transition into single mode
		updateStatusRole("single")
		log.Info("[decision] performActionFromMaster: other is dead: going single")
		actions <- "single"
		return
	}

	// see if im the odd man out or if it is the other guy
	time.Sleep(time.Duration(conf.DecisionTimeout) * time.Second)
	other, _ = Whoisnot(self.CRole)
	if other != nil {
		log.Info("[decision] performActionFromMaster: other came back: doing nothing")
		return
	}
	mon, _ := Whois("monitor")
	if mon != nil {
		// the other member died but i can still talk to the monitor
		// i can safely become a single
		updateStatusRole("single")
		log.Info("[decision] performActionFromMaster: other gone: going single")
		actions <- "single"
		return
	} else {
		// I have lost communication with everything else
		// kill my server
		updateStatusRole("dead(master)")
		log.Info("[decision] performActionFromMaster: lost connection to cluster: going dead")
		actions <- "kill"
		return
	}
}

// Breakdown of the performAction for when we are slave
func performActionFromSlave(self, other *Status) {
	if other != nil && other.DBRole == "master" {
		// i probably lost the monitor
		// shouldnt hurt anything
		log.Info("[decision] performActionFromSlave: other is master: im doing nothing")
		return
	}
	if other != nil && other.DBRole == "dead(master)" {

		// before i can become a single i need to make sure i was actually synced
		if self.State != "(slave)running" {
			updateStatusRole("dead(slave)")
			log.Info("[decision] performActionFromSlave: other is dead but I wasnt running: going dead")
			actions <- "kill"
			return
		}

		// my master has died and i need to transition into single mode
		updateStatusRole("single")
		log.Info("[decision] performActionFromSlave: other is dead: going single")
		actions <- "single"
		return
	}

	// see if im the odd man out or if it is the other guy
	time.Sleep(time.Duration(conf.DecisionTimeout) * time.Second)
	other, _ = Whoisnot(self.CRole)
	if other != nil {
		log.Info("[decision] performActionFromslave: other came back: doing nothing")
		return
	}
	mon, _ := Whois("monitor")
	if mon != nil {

		// before i can become a single i need to make sure i was actually synced
		if self.State != "(slave)running" {
			updateStatusRole("dead(slave)")
			log.Info("[decision] performActionFromSlave: other is dead but I wasnt running: going dead")
			actions <- "kill"
			return
		}

		// if the other CRole is still up according to the monitor I need to kill
		monitorKnows := false
		var other string

		switch self.CRole {
		case "primary":
			other = "secondary"
		case "secondary":
			other = "primary"
		}
		for _, memb := range Cluster("monitor") {
			if memb.CRole == other {
				monitorKnows = true
			}
		}

		if monitorKnows {
			updateStatusRole("dead(slave)")
			log.Info("[decision] performActionFromSlave: other is dead but I wasnt running: going dead")
			actions <- "kill"
			return
		}

		// the other member died but i can still talk to the monitor
		// i can safely become a single
		updateStatusRole("single")
		log.Info("[decision] performActionFromSlave: other gone: going single")
		actions <- "single"
		return
	} else {
		// I have lost communication with everything else
		// kill my server
		updateStatusRole("dead(slave)")
		log.Info("[decision] performActionFromSlave: lost connection to cluster: going dead")
		actions <- "kill"
		return
	}
}

// Breakdown of the performAction for when we are dead
func performActionFromDead(self, other *Status) {
	c := Cluster()
	if other != nil && len(c) == 3 {
		switch self.DBRole {
		case "dead(single)":
			log.Info("[decision] performActionFromDead: other online: going master")
			updateStatusRole("master")
			actions <- "master"
		case "dead(master)":
			newRole := startType("master")
			updateStatusRole(newRole)
			log.Info("[decision] performActionFromDead: other online: going " + newRole)
			actions <- newRole
		case "dead(slave)":
			newRole := startType("slave")
			updateStatusRole(newRole)
			log.Info("[decision] performActionFromDead: other online: going " + newRole)
			actions <- newRole
		default:
			panic("i dont know how to be a " + self.DBRole)
		}
	}
}

// when we udpate the DBROle we need to make sure
// to reflect this in the lastKnownCluster singleton
func updateStatusRole(r string) {
	status.SetDBRole(r)
	lastKnownCluster = Cluster()
}
