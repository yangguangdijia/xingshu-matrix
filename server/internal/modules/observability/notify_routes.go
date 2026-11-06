package observability

import (
	"github.com/gin-gonic/gin"

	"github.com/yangguangdijia/xingshu-matrix/server/internal/rbac"
)

// registerNotify mounts the notification center: channels, templates,
// subscriptions, send records and the outbound address policy. All routes
// require notify:manage. It replaces the channel routes previously declared
// in Register (see the module.go patch in the delivery report).
func (m *Module) registerNotify(g *gin.RouterGroup) {
	perm := rbac.RequirePerm(PermNotify)

	ch := g.Group("/channels", perm)
	ch.GET("", m.listChannels)
	ch.POST("", m.createChannel)
	ch.POST("/test", m.testChannelDraft)
	ch.GET("/:id", m.getChannel)
	ch.PUT("/:id", m.updateChannel)
	ch.PUT("/:id/enabled", m.setChannelEnabled)
	ch.GET("/:id/references", m.channelReferences)
	ch.DELETE("/:id", m.deleteChannel)
	ch.POST("/:id/test", m.testChannel)

	n := g.Group("/notify", perm)
	n.GET("/security", m.getNotifySecurity)
	n.PUT("/security", m.putNotifySecurity)

	n.GET("/templates", m.listTemplates)
	n.POST("/templates", m.createTemplate)
	n.POST("/templates/preview", m.previewTemplate)
	n.GET("/templates/:id", m.getTemplate)
	n.PUT("/templates/:id", m.updateTemplate)
	n.DELETE("/templates/:id", m.deleteTemplate)

	n.GET("/subscriptions", m.listSubscriptions)
	n.POST("/subscriptions", m.saveSubscription)
	n.POST("/subscriptions/match", m.matchSubscriptions)
	n.GET("/subscriptions/:id", m.getSubscription)
	n.PUT("/subscriptions/:id", m.saveSubscription)
	n.PUT("/subscriptions/:id/enabled", m.setSubscriptionEnabled)
	n.DELETE("/subscriptions/:id", m.deleteSubscription)

	n.GET("/records", m.listRecords)
	n.GET("/records/:id", m.getRecord)
	n.POST("/records/:id/resend", m.resendRecord)
}
