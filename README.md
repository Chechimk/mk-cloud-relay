# MK Cloud Relay

A protocol-agnostic HTTP relay for Google Cloud Run. It forwards HTTP/1.1,
HTTP/2, gRPC, WebSocket, HTTPUpgrade and XHTTP traffic to backends you
configure, without reading or changing the payload, and carries raw TCP
(for example an HTTP proxy's CONNECT, or SSH) over authenticated WebSocket
tunnels.

Work in progress: full documentation and verified Cloud Run results follow.
