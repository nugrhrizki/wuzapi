#! /usr/bin/env bash

# exit when any command fails
set -e

# Build the project.
go build .

# move the binary to the /usr/local/wuzapi directory

# check if the directory exists
if [ ! -d /usr/local/wuzapi ]; then
    # create the directory
    sudo mkdir -p /usr/local/wuzapi
fi

# copy the binary to the directory
sudo cp wuzapi /usr/local/wuzapi/wuzapi

# check if the service is already there
if [ -f /etc/systemd/system/wuzapi.service ]; then
    # stop the service
    sudo systemctl stop wuzapi.service
    # remove the service
    sudo rm /etc/systemd/system/GoWebApp.service
fi

# copy the service file
sudo cp wuzapi.service /etc/systemd/system/wuzapi.service

# reload the service
sudo systemctl daemon-reload

# start the service
sudo systemctl start wuzapi.service

# enable the service
sudo systemctl enable wuzapi.service
