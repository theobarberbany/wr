// Copyright © 2016 Genome Research Limited
// Author: Sendu Bala <sb10@sanger.ac.uk>.
//
//  This file is part of wr.
//
//  wr is free software: you can redistribute it and/or modify
//  it under the terms of the GNU Lesser General Public License as published by
//  the Free Software Foundation, either version 3 of the License, or
//  (at your option) any later version.
//
//  wr is distributed in the hope that it will be useful,
//  but WITHOUT ANY WARRANTY; without even the implied warranty of
//  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
//  GNU Lesser General Public License for more details.
//
//  You should have received a copy of the GNU Lesser General Public License
//  along with wr. If not, see <http://www.gnu.org/licenses/>.

package queue

import (
	"fmt"
	. "github.com/smartystreets/goconvey/convey"
	"testing"
	"time"
)

func TestDelayQueue(t *testing.T) {
	Convey("Once 10 items of differing delay have been pushed to the queue", t, func() {
		queue := newSubQueue(0)
		items := make(map[string]*Item)
		for i := 0; i < 10; i++ {
			key := fmt.Sprintf("key_%d", i)
			delay := time.Duration((9 - i + 1)) * time.Second
			items[key] = newItem(key, "", "data", 0, delay, 0*time.Second)
			queue.push(items[key])
		}

		So(queue.Len(), ShouldEqual, 10)

		Convey("Popping them should remove them in delay order", func() {
			exampleItem := items["key_1"]

			for i := 0; i < 5; i++ {
				item := queue.pop()
				So(item, ShouldHaveSameTypeAs, exampleItem)
				So(item.Key, ShouldEqual, fmt.Sprintf("key_%d", 9-i))
			}
			So(queue.Len(), ShouldEqual, 5)
			for i := 0; i < 5; i++ {
				item := queue.pop()
				So(item, ShouldHaveSameTypeAs, exampleItem)
				So(item.Key, ShouldEqual, fmt.Sprintf("key_%d", 9-i-5))
			}
			So(queue.Len(), ShouldEqual, 0)

			item := queue.pop()
			So(item, ShouldBeNil)
		})

		Convey("Removing an item works", func() {
			removeItem := items["key_2"]
			queue.remove(removeItem)
			So(queue.Len(), ShouldEqual, 9)

			for {
				item := queue.pop()
				if item == nil {
					break
				}
				So(item.Key, ShouldNotEqual, "key_2")
			}
			So(queue.Len(), ShouldEqual, 0)
		})

		Convey("Updating an item works", func() {
			exampleItem := items["key_9"]
			exampleItem.readyAt = time.Now().Add(2500 * time.Millisecond)
			queue.update(exampleItem)
			newItem := queue.pop()
			So(newItem.Key, ShouldEqual, "key_8")
		})

		Convey("Removing all items works", func() {
			queue.empty()
			So(queue.Len(), ShouldEqual, 0)
		})
	})
}
