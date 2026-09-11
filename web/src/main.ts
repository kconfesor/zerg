import { createApp } from 'vue'
import App from './App.vue'
import { router } from './router'
import './style.css'

const app = createApp(App).use(router)

// router.isReady() is what resolves the URL actually in the address bar,
// asynchronously, on first load. Mounting before it settles let App.vue's
// onMounted read `route.name` as whatever the router had not yet resolved
// to -- usually nothing, which viewOf falls back to "board" for -- and then
// write that back with router.replace as if it were the real destination.
// A direct link to any view other than the board landed there anyway,
// every time, with no error to explain why.
router.isReady().then(() => app.mount('#app'))
