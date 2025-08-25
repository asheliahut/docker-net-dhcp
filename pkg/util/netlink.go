package util

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

func AwaitNetNS(ctx context.Context, path string, interval time.Duration) (netns.NsHandle, error) {
	var err error
	nsChan := make(chan netns.NsHandle)
	go func() {
		for {
			var ns netns.NsHandle
			ns, err = netns.GetFromPath(path)
			if err == nil {
				nsChan <- ns
				return
			}

			time.Sleep(interval)
		}
	}()

	var dummy netns.NsHandle
	select {
	case ns := <-nsChan:
		return ns, nil
	case <-ctx.Done():
		if err != nil {
			log.WithError(err).WithField("path", path).Error("Failed to await network namespace")
		}
		return dummy, ctx.Err()
	}
}

func AwaitLinkByIndex(ctx context.Context, handle *netlink.Handle, index int, interval time.Duration) (netlink.Link, error) {
	var err error
	linkChan := make(chan netlink.Link)
	go func() {
		for {
			var link netlink.Link
			link, err = handle.LinkByIndex(index)
			if err == nil {
				linkChan <- link
				return
			}

			time.Sleep(interval)
		}
	}()

	var dummy netlink.Link
	select {
	case link := <-linkChan:
		return link, nil
	case <-ctx.Done():
		if err != nil {
			log.WithError(err).WithField("index", index).Error("Failed to await link by index")
		}
		return dummy, ctx.Err()
	}
}

// AwaitNetNSWithRetry waits for network namespace with custom retry logic
func AwaitNetNSWithRetry(ctx context.Context, path string, interval time.Duration, shouldRetry func(error) bool) (netns.NsHandle, error) {
	var lastErr error
	nsChan := make(chan netns.NsHandle)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				ns, err := netns.GetFromPath(path)
				if err == nil {
					nsChan <- ns
					return
				}

				lastErr = err
				if !shouldRetry(err) {
					return
				}

				time.Sleep(interval)
			}
		}
	}()

	var dummy netns.NsHandle
	select {
	case ns := <-nsChan:
		return ns, nil
	case <-ctx.Done():
		if lastErr != nil {
			log.WithError(lastErr).WithField("path", path).Error("Failed to await network namespace with retry")
			return dummy, lastErr
		}
		return dummy, ctx.Err()
	}
}
